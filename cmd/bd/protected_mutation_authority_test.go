package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/rhar1511/beads/internal/protectedmutation"
	"github.com/rhar1511/beads/internal/storage/domain"
	"github.com/rhar1511/beads/internal/storage/schema"
)

type testProtectedMutationReadinessSource struct {
	proof schema.ProtectedMutationReadiness
	err   error
}

func (s testProtectedMutationReadinessSource) ProtectedMutationReadiness(context.Context) (schema.ProtectedMutationReadiness, error) {
	return s.proof, s.err
}

func TestResolveServeProtectedMutationAuthorityIsAbsentByDefault(t *testing.T) {
	unsetServeAuthorityEnv(t)
	authority, closeAuthority, err := resolveServeProtectedMutationAuthority(context.Background(), domain.ContextInfo{}, nil)
	if err != nil {
		t.Fatalf("resolve absent authority: %v", err)
	}
	if !reflect.ValueOf(authority).IsZero() {
		t.Fatalf("absent authority = %#v, want disabled zero value", authority)
	}
	if err := closeAuthority(); err != nil {
		t.Fatalf("close absent authority: %v", err)
	}
}

func TestResolveServeProtectedMutationAuthorityFailsClosedWhenConfigured(t *testing.T) {
	t.Run("empty path", func(t *testing.T) {
		t.Setenv(serveProtectedMutationAuthorityEnv, "")
		_, _, err := resolveServeProtectedMutationAuthority(context.Background(), domain.ContextInfo{}, nil)
		if err == nil || !strings.Contains(err.Error(), "set but empty") {
			t.Fatalf("resolve error = %v, want explicit empty-setting error", err)
		}
	})

	t.Run("selected workspace mismatch", func(t *testing.T) {
		directory := t.TempDir()
		configPath := writeTestProtectedMutationConfig(t, directory, "project-a", "database-a")
		t.Setenv(serveProtectedMutationAuthorityEnv, configPath)
		_, _, err := resolveServeProtectedMutationAuthority(context.Background(), domain.ContextInfo{
			ProjectID: "project-b", Database: "database-a",
		}, nil)
		if err == nil || !strings.Contains(err.Error(), "does not match the selected workspace") {
			t.Fatalf("resolve error = %v, want project binding refusal", err)
		}
	})

	t.Run("missing signed files", func(t *testing.T) {
		directory := t.TempDir()
		configPath := writeTestProtectedMutationConfig(t, directory, "project-a", "database-a")
		t.Setenv(serveProtectedMutationAuthorityEnv, configPath)
		_, _, err := resolveServeProtectedMutationAuthority(context.Background(), domain.ContextInfo{
			ProjectID: "project-a", Database: "database-a",
		}, nil)
		if err == nil || !strings.Contains(err.Error(), "validate protected mutation authority before serving") {
			t.Fatalf("resolve error = %v, want missing authority files to fail before readiness", err)
		}
	})

	t.Run("schema readiness unavailable", func(t *testing.T) {
		directory := t.TempDir()
		configPath := writeTestProtectedMutationAuthorityFiles(t, directory, "project-a", "database-a")
		t.Setenv(serveProtectedMutationAuthorityEnv, configPath)
		_, _, err := resolveServeProtectedMutationAuthority(context.Background(), domain.ContextInfo{
			ProjectID: "project-a", Database: "database-a",
		}, nil)
		if err == nil || !strings.Contains(err.Error(), "does not expose protected mutation schema readiness") {
			t.Fatalf("resolve error = %v, want missing readiness source refusal", err)
		}
	})

	t.Run("schema not ready", func(t *testing.T) {
		directory := t.TempDir()
		configPath := writeTestProtectedMutationAuthorityFiles(t, directory, "project-a", "database-a")
		t.Setenv(serveProtectedMutationAuthorityEnv, configPath)
		_, _, err := resolveServeProtectedMutationAuthority(context.Background(), domain.ContextInfo{
			ProjectID: "project-a", Database: "database-a",
		}, testProtectedMutationReadinessSource{})
		if err == nil || !strings.Contains(err.Error(), "not ready for protected mutation enforcement") {
			t.Fatalf("resolve error = %v, want unready schema refusal", err)
		}
	})
}

func TestResolveServeProtectedMutationAuthorityAcceptsExactReadyWorkspace(t *testing.T) {
	directory := t.TempDir()
	configPath := writeTestProtectedMutationAuthorityFiles(t, directory, "project-a", "database-a")
	t.Setenv(serveProtectedMutationAuthorityEnv, configPath)

	authority, closeAuthority, err := resolveServeProtectedMutationAuthority(
		context.Background(),
		domain.ContextInfo{ProjectID: "project-a", Database: "database-a"},
		testProtectedMutationReadinessSource{proof: readyTestProtectedMutationSchema(t)},
	)
	if err != nil {
		t.Fatalf("resolve configured authority: %v", err)
	}
	if reflect.ValueOf(authority).IsZero() {
		t.Fatal("valid configured authority resolved to the disabled zero value")
	}
	if err := closeAuthority(); err != nil {
		t.Fatalf("close configured authority: %v", err)
	}
}

func readyTestProtectedMutationSchema(t *testing.T) schema.ProtectedMutationReadiness {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?")).
		WithArgs("schema_migrations").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(MAX(version), 0) FROM schema_migrations")).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(schema.ProtectedMutationMainMigration))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT issue_id, protection_class FROM protected_issue_records LIMIT 0")).
		WillReturnRows(sqlmock.NewRows([]string{"issue_id", "protection_class"}))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT audience, replay_id FROM protected_mutation_permit_uses LIMIT 0")).
		WillReturnRows(sqlmock.NewRows([]string{"audience", "replay_id"}))
	primaryKeyQuery := regexp.QuoteMeta(`SELECT column_name FROM information_schema.statistics
		WHERE table_schema = DATABASE() AND table_name = ? AND index_name = 'PRIMARY'
		ORDER BY seq_in_index`)
	mock.ExpectQuery(primaryKeyQuery).WithArgs("protected_issue_records").
		WillReturnRows(sqlmock.NewRows([]string{"column_name"}).AddRow("issue_id"))
	mock.ExpectQuery(primaryKeyQuery).WithArgs("protected_mutation_permit_uses").
		WillReturnRows(sqlmock.NewRows([]string{"column_name"}).AddRow("audience").AddRow("replay_id"))
	columnQuery := regexp.QuoteMeta(`SELECT data_type, character_maximum_length, is_nullable
		FROM information_schema.columns
		WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?`)
	for _, column := range []struct {
		table  string
		name   string
		length int64
	}{
		{table: "protected_issue_records", name: "issue_id", length: 255},
		{table: "protected_issue_records", name: "protection_class", length: 64},
		{table: "protected_mutation_permit_uses", name: "audience", length: 255},
		{table: "protected_mutation_permit_uses", name: "replay_id", length: 255},
	} {
		mock.ExpectQuery(columnQuery).WithArgs(column.table, column.name).
			WillReturnRows(sqlmock.NewRows([]string{"data_type", "character_maximum_length", "is_nullable"}).
				AddRow("varchar", column.length, "NO"))
	}
	proof, err := schema.ProbeProtectedMutationReadiness(context.Background(), db)
	if err != nil {
		t.Fatalf("probe protected mutation readiness: %v", err)
	}
	if !proof.Ready() {
		t.Fatal("complete protected mutation schema proof was not ready")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	return proof
}

func writeTestProtectedMutationConfig(t *testing.T, directory, projectID, database string) string {
	t.Helper()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	config := protectedmutation.HostAuthorityConfig{
		SchemaVersion: protectedmutation.HostAuthoritySchemaV1,
		Audience:      "audience-a", ProjectID: projectID, Database: database,
		KeyringFile: "keys.json", RevocationsFile: "revocations.json",
	}
	writeTestAuthorityJSON(t, filepath.Join(directory, "authority.json"), config)
	return filepath.Join(directory, "authority.json")
}

func writeTestProtectedMutationAuthorityFiles(t *testing.T, directory, projectID, database string) string {
	t.Helper()
	path, _ := writeTestProtectedMutationAuthorityAndKey(t, directory, projectID, database)
	return path
}

func writeTestProtectedMutationAuthorityAndKey(t *testing.T, directory, projectID, database string) (string, ed25519.PrivateKey) {
	t.Helper()
	configPath := writeTestProtectedMutationConfig(t, directory, projectID, database)
	permitPublic, permitPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	revocationPublic, revocationPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyring := protectedmutation.Keyring{
		SchemaVersion: protectedmutation.HostAuthoritySchemaV1,
		Generation:    1,
		Keys: []protectedmutation.TrustedKey{
			{KeyID: "permit-key", Issuer: "issuer-a", Purpose: protectedmutation.PermitKeyPurpose, PublicKey: base64.StdEncoding.EncodeToString(permitPublic)},
			{KeyID: "revocation-key", Issuer: "issuer-a", Purpose: protectedmutation.RevocationKeyPurpose, PublicKey: base64.StdEncoding.EncodeToString(revocationPublic)},
		},
	}
	writeTestAuthorityJSON(t, filepath.Join(directory, "keys.json"), keyring)
	now := time.Now().UTC().Truncate(time.Second)
	revocations := protectedmutation.Revocations{
		SchemaVersion: protectedmutation.RevocationsSchemaVersionV1,
		Purpose:       protectedmutation.RevocationKeyPurpose,
		KeyID:         "revocation-key", Issuer: "issuer-a", Audience: "audience-a",
		ProjectID: projectID, Database: database, Generation: 1,
		IssuedAt: now.Format(time.RFC3339Nano), ExpiresAt: now.Add(4 * time.Minute).Format(time.RFC3339Nano),
		RevokedReplayIDs: []string{},
	}
	signingBytes, err := protectedmutation.RevocationSigningBytes(revocations)
	if err != nil {
		t.Fatalf("canonical revocation bytes: %v", err)
	}
	const domain = protectedmutation.RevocationSigningDomain
	payload := signingBytes[len(domain):]
	signed := protectedmutation.SignedRevocations{
		Payload:   json.RawMessage(payload),
		Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(revocationPrivate, signingBytes)),
	}
	writeTestAuthorityJSON(t, filepath.Join(directory, "revocations.json"), signed)
	return configPath, permitPrivate
}

func writeTestAuthorityJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func unsetServeAuthorityEnv(t *testing.T) {
	t.Helper()
	previous, present := os.LookupEnv(serveProtectedMutationAuthorityEnv)
	if err := os.Unsetenv(serveProtectedMutationAuthorityEnv); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if present {
			_ = os.Setenv(serveProtectedMutationAuthorityEnv, previous)
		} else {
			_ = os.Unsetenv(serveProtectedMutationAuthorityEnv)
		}
	})
}
