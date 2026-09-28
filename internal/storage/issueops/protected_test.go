package issueops

import (
	"context"
	"database/sql/driver"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	publicops "github.com/steveyegge/beads/issueops"
)

func expectNoProtectedRecords(mock sqlmock.Sqlmock, ids ...string) {
	ids = canonicalProtectedIDs(ids)
	args := make([]driver.Value, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	mock.ExpectQuery(regexp.QuoteMeta("SELECT issue_id FROM protected_issue_records WHERE protection_class <> '' AND issue_id IN (" + placeholders + ") ORDER BY issue_id")).
		WithArgs(args...).
		WillReturnRows(sqlmock.NewRows([]string{"issue_id"}))
}

type protectedMutationVerifierFunc func(context.Context, publicops.ProtectedMutationPermit, publicops.ProtectedMutationScope) (publicops.ProtectedMutationVerification, error)

func (f protectedMutationVerifierFunc) Verify(ctx context.Context, permit publicops.ProtectedMutationPermit, scope publicops.ProtectedMutationScope) (publicops.ProtectedMutationVerification, error) {
	return f(ctx, permit, scope)
}

func TestRequireProtectedMutationPassesExactScopeToVerifier(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectBegin()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT issue_id FROM protected_issue_records WHERE protection_class <> '' AND issue_id IN (?,?,?) ORDER BY issue_id")).
		WithArgs("alpha", "beta", "zeta").
		WillReturnRows(sqlmock.NewRows([]string{"issue_id"}).AddRow("alpha"))
	mock.ExpectExec(`INSERT INTO protected_mutation_permit_uses\s+\(audience, replay_id\) VALUES \(\?, \?\)`).
		WithArgs("workspace-audience", "unique-use-1").
		WillReturnResult(sqlmock.NewResult(1, 1))

	var got publicops.ProtectedMutationScope
	var gotPermit publicops.ProtectedMutationPermit
	verifier := protectedMutationVerifierFunc(func(_ context.Context, permit publicops.ProtectedMutationPermit, scope publicops.ProtectedMutationScope) (publicops.ProtectedMutationVerification, error) {
		got, gotPermit = scope, permit
		return publicops.ProtectedMutationVerification{ReplayID: "unique-use-1"}, nil
	})
	ctx := WithProtectedMutationVerifier(context.Background(), verifier, "workspace-audience")
	err = RequireProtectedMutation(ctx, tx, "signed-token", "issue.update", []string{"zeta", "alpha", "beta", "alpha"}, "canonical-digest", "")
	if err != nil {
		t.Fatalf("RequireProtectedMutation: %v", err)
	}
	if gotPermit != "signed-token" {
		t.Fatalf("permit = %q, want request permit", gotPermit)
	}
	if got.Audience != "workspace-audience" || got.Operation != "issue.update" || got.Digest != "canonical-digest" {
		t.Fatalf("scope = %#v, want exact audience, operation, and digest", got)
	}
	wantIDs := []string{"alpha", "beta", "zeta"}
	if len(got.ResourceIDs) != len(wantIDs) {
		t.Fatalf("scope resource IDs = %#v, want %#v", got.ResourceIDs, wantIDs)
	}
	for i := range wantIDs {
		if got.ResourceIDs[i] != wantIDs[i] {
			t.Fatalf("scope resource IDs = %#v, want %#v", got.ResourceIDs, wantIDs)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRequireProtectedMutationConsumesReplayIdentityInActiveTransaction(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectBegin()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT issue_id FROM protected_issue_records WHERE protection_class <> '' AND issue_id IN (?) ORDER BY issue_id")).
		WithArgs("protected").
		WillReturnRows(sqlmock.NewRows([]string{"issue_id"}).AddRow("protected"))
	mock.ExpectExec(`INSERT INTO protected_mutation_permit_uses\s+\(audience, replay_id\) VALUES \(\?, \?\)`).
		WithArgs("workspace-audience", "replay-1").
		WillReturnError(errors.New("duplicate key"))

	verifier := protectedMutationVerifierFunc(func(context.Context, publicops.ProtectedMutationPermit, publicops.ProtectedMutationScope) (publicops.ProtectedMutationVerification, error) {
		return publicops.ProtectedMutationVerification{ReplayID: "replay-1"}, nil
	})
	ctx := WithProtectedMutationVerifier(context.Background(), verifier, "workspace-audience")
	err = RequireProtectedMutation(ctx, tx, "permit", "issue.delete", []string{"protected"}, "digest", "")
	if !errors.Is(err, publicops.ErrProtectedRecord) {
		t.Fatalf("RequireProtectedMutation error = %v, want ErrProtectedRecord on replay", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRequireProtectedMutationFailsClosedWithoutVerifier(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectBegin()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT issue_id FROM protected_issue_records WHERE protection_class <> '' AND issue_id IN (?) ORDER BY issue_id")).
		WithArgs("protected").
		WillReturnRows(sqlmock.NewRows([]string{"issue_id"}).AddRow("protected"))

	err = RequireProtectedMutation(context.Background(), tx, "", "issue.delete", []string{"protected"}, "", "")
	if !errors.Is(err, publicops.ErrProtectedRecord) {
		t.Fatalf("RequireProtectedMutation error = %v, want ErrProtectedRecord", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
