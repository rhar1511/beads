package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/rhar1511/beads/internal/storage/domain"
	"github.com/rhar1511/beads/internal/storage/schema"
	"github.com/rhar1511/beads/issueops"
)

const transitionPatchBody = `{"receipt_id":"patch-1","scope":"q54","kind":"route","actor":" alice ","expected_version":"1","prior_receipt_id":"parent-1","prior_receipt_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","patch":{"metadata":[{"key":"gc.route","expected":null,"value":"ready"}],"status":{"expected":"open","value":"in_progress"},"assignee":{"expected":"","value":"worker-1"},"labels":{"expected":["old"],"value":["new"]}},"protected_permit":"signed-patch-permit"}`

type revisionTransitionPatchTestRole struct {
	*revisionTransitionTestRole
	request issueops.RevisionTransitionPatchRequest
	receipt *issueops.RevisionTransitionPatchReceipt
	calls   int
}

func (r *revisionTransitionPatchTestRole) TransitionPatch(_ context.Context, request issueops.RevisionTransitionPatchRequest) (issueops.RevisionTransitionPatchResult, error) {
	r.calls++
	r.request = request
	if r.receipt == nil {
		r.receipt = &issueops.RevisionTransitionPatchReceipt{
			ReceiptID: request.ReceiptID, IssueID: request.IssueID, Scope: request.Scope,
			Kind: request.Kind, Actor: request.Actor, ExpectedVersion: request.ExpectedVersion,
			ToVersion: 987654321, PriorReceiptID: request.PriorReceiptID,
			PriorReceiptDigest: request.PriorReceiptDigest, Patch: request.Patch,
		}
	}
	return issueops.RevisionTransitionPatchResult{Applied: true, Receipt: r.receipt}, nil
}

func (r *revisionTransitionPatchTestRole) GetPatchReceipt(_ context.Context, receiptID string) (*issueops.RevisionTransitionPatchReceipt, error) {
	if r.receipt == nil || r.receipt.ReceiptID != receiptID {
		return nil, errors.New("missing patch receipt")
	}
	return r.receipt, nil
}

func TestTransitionPatchRequiresSchemaRoleAndProtectedAuthority(t *testing.T) {
	readiness := readyTransitionPatchReadinessProof(t)
	protectedReadiness := readyProtectedMutationReadiness(t)
	authority, err := NewProtectedMutationAuthority(testProtectedMutationVerifier{}, "audience-a", "project-a", "database-a", protectedReadiness)
	if err != nil {
		t.Fatalf("construct protected authority: %v", err)
	}
	for _, tc := range []struct {
		name          string
		readiness     schema.RevisionTransitionReadiness
		withPatchRole bool
		withAuthority bool
	}{
		{name: "schema missing", readiness: schema.RevisionTransitionReadiness{}, withPatchRole: true, withAuthority: true},
		{name: "patch role missing", readiness: readiness, withPatchRole: false, withAuthority: true},
		{name: "protected verifier missing", readiness: readiness, withPatchRole: true, withAuthority: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := &revisionTransitionTestRole{}
			var role issueops.RevisionTransition = base
			if tc.withPatchRole {
				role = &revisionTransitionPatchTestRole{revisionTransitionTestRole: base}
			}
			cfg := Config{
				Auth: authTokenFile(t, "token"), RevisionTransition: role,
				TransitionReadiness: tc.readiness,
				Workspace:           domain.ContextInfo{ProjectID: "project-a", Database: "database-a"},
			}
			if tc.withAuthority {
				cfg.ProtectedMutationAuthority = authority
			}
			server := newTestServer(t, rolesConfig(cfg))
			if server.revisionPatchReady {
				t.Fatal("patch route became ready with an incomplete proof")
			}
			contextBody := decodeBody(t, server.do(t, http.MethodGet, "/v0/beads/context", bearer("token")))
			caps, ok := contextBody["capabilities"].([]any)
			if !ok || hasAnyString(caps, CapTransitionPatch, CapTransitionPatchReceipt) {
				t.Fatalf("context advertised incomplete patch capability pair: %v", contextBody["capabilities"])
			}
			headers := bearer("token")
			headers.Set(ProjectIDHeader, "project-a")
			response := server.doBody(t, http.MethodPost, "/v0/beads/issues/bd-1:transitionPatch", headers, "application/json", transitionPatchBody)
			if response.StatusCode != http.StatusNotImplemented {
				t.Fatalf("unready patch status = %d, want 501", response.StatusCode)
			}
			if patchRole, ok := role.(*revisionTransitionPatchTestRole); ok && patchRole.calls != 0 {
				t.Fatal("unready patch reached the role")
			}
		})
	}
}

func TestTransitionPatchRouteCarriesExactPermitAndReceiptChain(t *testing.T) {
	readiness := readyTransitionPatchReadinessProof(t)
	protectedReadiness := readyProtectedMutationReadiness(t)
	authority, err := NewProtectedMutationAuthority(testProtectedMutationVerifier{}, "audience-a", "project-a", "database-a", protectedReadiness)
	if err != nil {
		t.Fatalf("construct protected authority: %v", err)
	}
	base := &revisionTransitionTestRole{}
	role := &revisionTransitionPatchTestRole{revisionTransitionTestRole: base}
	server := newTestServer(t, rolesConfig(Config{
		Auth: authTokenFile(t, "token"), RevisionTransition: role,
		TransitionReadiness: readiness, ProtectedMutationAuthority: authority,
		Workspace: domain.ContextInfo{ProjectID: "project-a", Database: "database-a"},
	}))
	if !server.revisionPatchReady {
		t.Fatal("complete patch and protected readiness did not enable patch routes")
	}
	contextBody := decodeBody(t, server.do(t, http.MethodGet, "/v0/beads/context", bearer("token")))
	caps, ok := contextBody["capabilities"].([]any)
	if !ok || !hasString(caps, CapTransitionPatch) || !hasString(caps, CapTransitionPatchReceipt) {
		t.Fatalf("ready context omitted patch capability pair: %v", contextBody["capabilities"])
	}
	headers := bearer("token")
	headers.Set(ProjectIDHeader, "project-a")
	response := server.doBody(t, http.MethodPost, "/v0/beads/issues/bd-1:transitionPatch", headers, "application/json", transitionPatchBody)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("patch status = %d: %s", response.StatusCode, readAll(t, response))
	}
	if role.calls != 1 || role.request.ProtectedPermit != "signed-patch-permit" || role.request.Actor != "alice" ||
		role.request.IssueID != "bd-1" || role.request.ExpectedVersion != 1 || role.request.PriorReceiptID != "parent-1" ||
		role.request.Patch.Metadata[0].Expected == nil || string(*role.request.Patch.Metadata[0].Expected) != "null" {
		t.Fatalf("backend request lost route/permit/chain/null binding: %+v", role.request)
	}
	body := decodeBody(t, response)
	receipt, ok := body["receipt"].(map[string]any)
	if !ok || receipt["expected_version"] != "1" || receipt["to_version"] != "987654321" || receipt["prior_receipt_id"] != "parent-1" {
		t.Fatalf("patch response receipt = %v, want canonical revision strings and parent link", body["receipt"])
	}
	get := server.do(t, http.MethodGet, "/v0/beads/issue-transition-patch-receipts/patch-1", headers)
	if get.StatusCode != http.StatusOK {
		t.Fatalf("patch receipt GET status = %d: %s", get.StatusCode, readAll(t, get))
	}
}

func TestTransitionPatchParserRejectsUnknownNestedMembers(t *testing.T) {
	readiness := readyTransitionPatchReadinessProof(t)
	protectedReadiness := readyProtectedMutationReadiness(t)
	authority, err := NewProtectedMutationAuthority(testProtectedMutationVerifier{}, "audience-a", "project-a", "database-a", protectedReadiness)
	if err != nil {
		t.Fatalf("construct protected authority: %v", err)
	}
	role := &revisionTransitionPatchTestRole{revisionTransitionTestRole: &revisionTransitionTestRole{}}
	server := newTestServer(t, rolesConfig(Config{
		Auth: authTokenFile(t, "token"), RevisionTransition: role,
		TransitionReadiness: readiness, ProtectedMutationAuthority: authority,
		Workspace: domain.ContextInfo{ProjectID: "project-a", Database: "database-a"},
	}))
	badBody := strings.Replace(transitionPatchBody, `"value":"ready"`, `"value":"ready","unexpected":true`, 1)
	headers := bearer("token")
	headers.Set(ProjectIDHeader, "project-a")
	response := server.doBody(t, http.MethodPost, "/v0/beads/issues/bd-1:transitionPatch", headers, "application/json", badBody)
	if response.StatusCode != http.StatusBadRequest || role.calls != 0 {
		t.Fatalf("nested unknown field status=%d role calls=%d, want 400 and no role call", response.StatusCode, role.calls)
	}
}
