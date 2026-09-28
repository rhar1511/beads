package protectedmutation

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/issueops"
)

type memoryAuthoritySource struct{ bundle Bundle }

func (s memoryAuthoritySource) Load(context.Context) (Bundle, error) { return s.bundle, nil }

func TestVerifierBindsSignedPermitToExactScopeAndCurrentRevocations(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	fixture := newAuthorityFixture(t, now, nil)
	verifier, err := NewVerifier(memoryAuthoritySource{bundle: fixture.bundle}, Options{
		Audience: "audience-a", ProjectID: "project-a", Database: "database-a", Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	if err := verifier.ValidateStartup(context.Background()); err != nil {
		t.Fatalf("ValidateStartup: %v", err)
	}

	scope := issueops.ProtectedMutationScope{
		Audience: "audience-a", ProjectID: "project-a", Database: "database-a",
		Operation: "issue.update", ResourceIDs: []string{"bd-2", "bd-1"}, Digest: fixture.digest,
	}
	got, err := verifier.Verify(context.Background(), issueops.ProtectedMutationPermit(fixture.token), scope)
	if err != nil {
		t.Fatalf("Verify valid permit: %v", err)
	}
	if got.ReplayID != fixture.replayID {
		t.Fatalf("ReplayID = %q, want stable signed replay ID %q", got.ReplayID, fixture.replayID)
	}

	cases := []struct {
		name   string
		mutate func(*issueops.ProtectedMutationScope)
	}{
		{name: "audience", mutate: func(s *issueops.ProtectedMutationScope) { s.Audience = "audience-b" }},
		{name: "project", mutate: func(s *issueops.ProtectedMutationScope) { s.ProjectID = "project-b" }},
		{name: "database", mutate: func(s *issueops.ProtectedMutationScope) { s.Database = "database-b" }},
		{name: "operation", mutate: func(s *issueops.ProtectedMutationScope) { s.Operation = "issue.close" }},
		{name: "resource set", mutate: func(s *issueops.ProtectedMutationScope) { s.ResourceIDs = []string{"bd-1", "bd-3"} }},
		{name: "digest", mutate: func(s *issueops.ProtectedMutationScope) { s.Digest = testDigest("other request") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := scope
			tc.mutate(&want)
			if _, err := verifier.Verify(context.Background(), issueops.ProtectedMutationPermit(fixture.token), want); err == nil {
				t.Fatalf("Verify accepted permit with mismatched %s", tc.name)
			}
		})
	}
}

func TestVerifierRejectsInvalidSignatureExpiredGrantAndRevocation(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name        string
		grantIssued time.Time
		grantExpiry time.Time
		revoked     bool
		badSig      bool
	}{
		{name: "expired grant", grantIssued: now.Add(-20 * time.Minute), grantExpiry: now.Add(-10 * time.Minute)},
		{name: "revoked replay ID", grantIssued: now.Add(-time.Minute), grantExpiry: now.Add(time.Minute), revoked: true},
		{name: "invalid signature", grantIssued: now.Add(-time.Minute), grantExpiry: now.Add(time.Minute), badSig: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newAuthorityFixture(t, now, func(claims *Claims, revoked *[]string) {
				claims.IssuedAt = tc.grantIssued.UTC().Format(time.RFC3339Nano)
				claims.ExpiresAt = tc.grantExpiry.UTC().Format(time.RFC3339Nano)
				if tc.revoked {
					*revoked = append(*revoked, claims.ReplayID)
				}
			})
			if tc.badSig {
				parts := strings.Split(fixture.token, ".")
				if parts[1][0] == 'A' {
					parts[1] = "B" + parts[1][1:]
				} else {
					parts[1] = "A" + parts[1][1:]
				}
				fixture.token = parts[0] + "." + parts[1]
			}
			verifier, err := NewVerifier(memoryAuthoritySource{bundle: fixture.bundle}, Options{
				Audience: "audience-a", ProjectID: "project-a", Database: "database-a", Now: func() time.Time { return now },
			})
			if err != nil {
				t.Fatalf("NewVerifier: %v", err)
			}
			scope := issueops.ProtectedMutationScope{
				Audience: "audience-a", ProjectID: "project-a", Database: "database-a",
				Operation: "issue.update", ResourceIDs: []string{"bd-1", "bd-2"}, Digest: fixture.digest,
			}
			if _, err := verifier.Verify(context.Background(), issueops.ProtectedMutationPermit(fixture.token), scope); err == nil {
				t.Fatal("Verify accepted invalid authority data")
			}
		})
	}
}

func TestVerifierStartupRejectsInvalidOrStaleRevocationSnapshot(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	fixture := newAuthorityFixture(t, now, nil)
	fixture.bundle.Revocations.Signature = "invalid"
	verifier, err := NewVerifier(memoryAuthoritySource{bundle: fixture.bundle}, Options{
		Audience: "audience-a", ProjectID: "project-a", Database: "database-a", Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	if err := verifier.ValidateStartup(context.Background()); err == nil {
		t.Fatal("ValidateStartup accepted an invalid revocation signature")
	}

	fixture = newAuthorityFixture(t, now, nil)
	fixture.bundle.Revocations = signRevocations(t, fixture.revocationPrivate, Revocations{
		SchemaVersion: RevocationsSchemaVersionV1, Purpose: RevocationKeyPurpose,
		KeyID: "revocation-key", Issuer: "issuer-a", Audience: "audience-a",
		ProjectID: "project-a", Database: "database-a", Generation: fixture.bundle.Generation,
		IssuedAt:         now.Add(-time.Hour).Format(time.RFC3339Nano),
		ExpiresAt:        now.Add(-time.Minute).Format(time.RFC3339Nano),
		RevokedReplayIDs: []string{},
	})
	verifier, err = NewVerifier(memoryAuthoritySource{bundle: fixture.bundle}, Options{
		Audience: "audience-a", ProjectID: "project-a", Database: "database-a", Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewVerifier for stale snapshot: %v", err)
	}
	if err := verifier.ValidateStartup(context.Background()); !errors.Is(err, ErrExpired) {
		t.Fatalf("ValidateStartup error = %v, want expired snapshot", err)
	}
}

func TestVerifierStartupRejectsDuplicateMembersInSignedPayload(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	fixture := newAuthorityFixture(t, now, nil)
	payload := string(fixture.bundle.Revocations.Payload)
	duplicate := strings.Replace(payload,
		`"purpose":"protected_mutation_revocation"`,
		`"purpose":"protected_mutation_revocation","purpose":"protected_mutation_revocation"`, 1)
	if duplicate == payload {
		t.Fatal("test payload did not contain the expected purpose member")
	}
	fixture.bundle.Revocations.Payload = json.RawMessage(duplicate)
	fixture.bundle.Revocations.Signature = base64.RawURLEncoding.EncodeToString(
		ed25519.Sign(fixture.revocationPrivate, append([]byte(RevocationSigningDomain), []byte(duplicate)...)),
	)
	verifier, err := NewVerifier(memoryAuthoritySource{bundle: fixture.bundle}, Options{
		Audience: "audience-a", ProjectID: "project-a", Database: "database-a", Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	if err := verifier.ValidateStartup(context.Background()); !errors.Is(err, ErrMalformed) {
		t.Fatalf("ValidateStartup error = %v, want duplicate-member ErrMalformed", err)
	}
}

func TestCanonicalSigningBytesAreStableAndStrictJSONRejectsDuplicateMembers(t *testing.T) {
	claims := Claims{
		SchemaVersion: SchemaVersionV1, Purpose: PermitKeyPurpose, KeyID: "grant-key",
		Issuer: "issuer-a", Audience: "audience-a", ProjectID: "project-a", Database: "database-a",
		Operation: "issue.update", ResourceIDs: []string{"bd-1", "bd-2"}, RequestDigest: testDigest("body"),
		ReplayID: "replay-id-00000001", IssuedAt: "2026-09-28T12:00:00Z", ExpiresAt: "2026-09-28T12:01:00Z",
	}
	got, err := GrantSigningBytes(claims)
	if err != nil {
		t.Fatalf("GrantSigningBytes: %v", err)
	}
	want := PermitSigningDomain + `{"schema_version":"beads.protected-mutation-permit.v1","purpose":"protected_mutation_permit","key_id":"grant-key","issuer":"issuer-a","audience":"audience-a","project_id":"project-a","database":"database-a","operation":"issue.update","resource_ids":["bd-1","bd-2"],"request_digest":"` + testDigest("body") + `","replay_id":"replay-id-00000001","issued_at":"2026-09-28T12:00:00Z","expires_at":"2026-09-28T12:01:00Z"}`
	if string(got) != want {
		t.Fatalf("signing bytes differ from the documented canonical grammar:\n got: %s\nwant: %s", got, want)
	}

	var target struct {
		Value string `json:"value"`
	}
	if err := decodeStrictJSON([]byte(`{"value":"first","value":"second"}`), &target); !errors.Is(err, ErrMalformed) {
		t.Fatalf("duplicate JSON member error = %v, want ErrMalformed", err)
	}
}

type authorityFixture struct {
	bundle            Bundle
	token             string
	digest            string
	replayID          string
	revocationPrivate ed25519.PrivateKey
}

func newAuthorityFixture(t *testing.T, now time.Time, mutate func(*Claims, *[]string)) authorityFixture {
	t.Helper()
	grantPublic, grantPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	revocationPublic, revocationPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	digest := testDigest("request body")
	replayID := "replay-id-00000001"
	claims := Claims{
		SchemaVersion: SchemaVersionV1, Purpose: PermitKeyPurpose, KeyID: "grant-key",
		Issuer: "issuer-a", Audience: "audience-a", ProjectID: "project-a", Database: "database-a",
		Operation: "issue.update", ResourceIDs: []string{"bd-1", "bd-2"}, RequestDigest: digest,
		ReplayID: replayID, IssuedAt: now.Add(-time.Minute).Format(time.RFC3339Nano),
		ExpiresAt: now.Add(time.Minute).Format(time.RFC3339Nano),
	}
	revoked := []string{}
	if mutate != nil {
		mutate(&claims, &revoked)
	}
	token := signPermit(t, grantPrivate, claims)
	keys := []TrustedKey{
		{KeyID: "grant-key", Issuer: "issuer-a", Purpose: PermitKeyPurpose, PublicKey: base64.StdEncoding.EncodeToString(grantPublic)},
		{KeyID: "revocation-key", Issuer: "issuer-a", Purpose: RevocationKeyPurpose, PublicKey: base64.StdEncoding.EncodeToString(revocationPublic)},
	}
	return authorityFixture{
		bundle: Bundle{
			Generation: 1,
			Keys:       keys,
			Revocations: signRevocations(t, revocationPrivate, Revocations{
				SchemaVersion: RevocationsSchemaVersionV1, Purpose: RevocationKeyPurpose,
				KeyID: "revocation-key", Issuer: "issuer-a", Audience: "audience-a",
				ProjectID: "project-a", Database: "database-a", Generation: 1,
				IssuedAt:  now.Add(-time.Minute).Format(time.RFC3339Nano),
				ExpiresAt: now.Add(time.Minute).Format(time.RFC3339Nano), RevokedReplayIDs: revoked,
			}),
		},
		token: token, digest: digest, replayID: replayID, revocationPrivate: revocationPrivate,
	}
}

func signPermit(t *testing.T, private ed25519.PrivateKey, claims Claims) string {
	t.Helper()
	payload, err := canonicalClaims(claims)
	if err != nil {
		t.Fatal(err)
	}
	signature := ed25519.Sign(private, append([]byte(PermitSigningDomain), payload...))
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func signRevocations(t *testing.T, private ed25519.PrivateKey, list Revocations) SignedRevocations {
	t.Helper()
	payload, err := canonicalRevocations(list)
	if err != nil {
		t.Fatal(err)
	}
	signature := ed25519.Sign(private, append([]byte(RevocationSigningDomain), payload...))
	return SignedRevocations{Payload: json.RawMessage(payload), Signature: base64.RawURLEncoding.EncodeToString(signature)}
}

func testDigest(text string) string {
	digest := sha256.Sum256([]byte(text))
	return hex.EncodeToString(digest[:])
}
