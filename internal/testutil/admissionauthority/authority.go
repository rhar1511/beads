// Package admissionauthority supplies ephemeral signed authorities to backend
// contract tests. Its in-memory keys never configure a running Beads service.
package admissionauthority

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rhar1511/beads/internal/protectedmutation"
	storeops "github.com/rhar1511/beads/internal/storage/issueops"
	publicops "github.com/rhar1511/beads/issueops"
)

type source struct{ bundle protectedmutation.Bundle }

func (s source) Load(context.Context) (protectedmutation.Bundle, error) { return s.bundle, nil }

// Authority binds real signed permits to a fixed test clock and workspace.
type Authority struct {
	Verifier publicops.ProtectedMutationVerifier
	private  ed25519.PrivateKey
	now      time.Time
}

// New uses separate, randomly generated permit and revocation signing keys.
func New(t *testing.T) *Authority {
	t.Helper()
	pub, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	revPub, revPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	list := protectedmutation.Revocations{SchemaVersion: protectedmutation.RevocationsSchemaVersionV1, Purpose: protectedmutation.RevocationKeyPurpose, KeyID: "revocation", Issuer: "contract", Audience: "contract", ProjectID: "contract", Database: "contract", Generation: 1, IssuedAt: now.Add(-time.Minute).Format(time.RFC3339Nano), ExpiresAt: now.Add(time.Minute).Format(time.RFC3339Nano), RevokedReplayIDs: []string{}}
	data, err := protectedmutation.RevocationSigningBytes(list)
	if err != nil {
		t.Fatal(err)
	}
	bundle := protectedmutation.Bundle{Generation: 1, Keys: []protectedmutation.TrustedKey{
		{KeyID: "permit", Issuer: "contract", Purpose: protectedmutation.PermitKeyPurpose, PublicKey: base64.StdEncoding.EncodeToString(pub)},
		{KeyID: "revocation", Issuer: "contract", Purpose: protectedmutation.RevocationKeyPurpose, PublicKey: base64.StdEncoding.EncodeToString(revPub)},
	}, Revocations: protectedmutation.SignedRevocations{Payload: json.RawMessage(strings.TrimPrefix(string(data), protectedmutation.RevocationSigningDomain)), Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(revPrivate, data))}}
	v, err := protectedmutation.NewVerifier(source{bundle}, protectedmutation.Options{Audience: "contract", ProjectID: "contract", Database: "contract", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if err := v.ValidateStartup(t.Context()); err != nil {
		t.Fatal(err)
	}
	return &Authority{Verifier: v, private: private, now: now}
}

// Authorizer signs the exact request digest and supplies the normal trusted
// backend context. The storage implementation must verify and consume it.
func (a *Authority) Authorizer(t *testing.T) func(context.Context, publicops.RevisionTransitionPatchRequest) (context.Context, publicops.ProtectedMutationPermit, publicops.ProtectedMutationScope) {
	t.Helper()
	return func(ctx context.Context, request publicops.RevisionTransitionPatchRequest) (context.Context, publicops.ProtectedMutationPermit, publicops.ProtectedMutationScope) {
		digest, err := publicops.ProtectedMutationDigest(request)
		if err != nil {
			t.Fatal(err)
		}
		scope := publicops.ProtectedMutationScope{Audience: "contract", ProjectID: "contract", Database: "contract", Operation: "issue.revision_transition_patch", ResourceIDs: []string{request.IssueID}, Digest: digest}
		claims := protectedmutation.Claims{SchemaVersion: protectedmutation.SchemaVersionV1, Purpose: protectedmutation.PermitKeyPurpose, KeyID: "permit", Issuer: "contract", Audience: scope.Audience, ProjectID: scope.ProjectID, Database: scope.Database, Operation: scope.Operation, ResourceIDs: scope.ResourceIDs, RequestDigest: scope.Digest, ReplayID: "contract-permit-" + request.ReceiptID, IssuedAt: a.now.Add(-time.Minute).Format(time.RFC3339Nano), ExpiresAt: a.now.Add(time.Minute).Format(time.RFC3339Nano)}
		data, err := protectedmutation.GrantSigningBytes(claims)
		if err != nil {
			t.Fatal(err)
		}
		payload := strings.TrimPrefix(string(data), protectedmutation.PermitSigningDomain)
		permit := base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(a.private, data))
		return storeops.WithProtectedMutationVerifier(ctx, a.Verifier, scope.Audience, scope.ProjectID, scope.Database), publicops.ProtectedMutationPermit(permit), scope
	}
}
