package httpapi

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/storage/domain"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/issueops"
)

type sourceSnapshotTestReader struct {
	snapshot *issueops.SourceSnapshot
	err      error
	calls    int
	issueID  string
}

func (r *sourceSnapshotTestReader) ReadSourceSnapshot(_ context.Context, issueID string) (*issueops.SourceSnapshot, error) {
	r.calls++
	r.issueID = issueID
	return r.snapshot, r.err
}

func TestSourceSnapshotRoutePublishesOnlyWhenReadyAndReturnsExplicitEmptyEdges(t *testing.T) {
	readiness := readyTransitionReadinessProof(t)
	role := &sourceSnapshotTestReader{snapshot: &issueops.SourceSnapshot{
		Issue:                &types.Issue{ID: "bd-1", Title: "repair", Priority: 1, RowVersion: -9223372036854775808},
		OutgoingDependencies: make([]*types.Dependency, 0),
	}}
	ts := newTestServer(t, rolesConfig(Config{
		Auth: authTokenFile(t, "token"), SourceSnapshotReader: role,
		TransitionReadiness: readiness, Workspace: domain.ContextInfo{ProjectID: "project-a"},
	}))
	if !ts.sourceSnapshotReady {
		t.Fatal("Listen did not enable the source snapshot route after every gate passed")
	}
	contextResponse := ts.do(t, http.MethodGet, "/v0/beads/context", bearer("token"))
	capabilities, ok := decodeBody(t, contextResponse)["capabilities"].([]any)
	if !ok || !hasString(capabilities, "issues.sourceSnapshot") {
		t.Fatalf("context capabilities = %v, want issues.sourceSnapshot", capabilities)
	}

	headers := bearer("token")
	headers.Set(ProjectIDHeader, "project-a")
	response := ts.do(t, http.MethodGet, "/v0/beads/issues/bd-1/sourceSnapshot", headers)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET source snapshot status = %d, want 200: %s", response.StatusCode, readAll(t, response))
	}
	body := decodeBody(t, response)
	issue, ok := body["issue"].(map[string]any)
	if !ok || issue["revision"] != "-9223372036854775808" {
		t.Errorf("issue revision = %v, want exact signed-decimal string", body["issue"])
	}
	edges, ok := body["outgoing_dependencies"].([]any)
	if !ok || edges == nil || len(edges) != 0 {
		t.Errorf("outgoing_dependencies = %#v, want explicit empty array", body["outgoing_dependencies"])
	}
	if role.calls != 1 || role.issueID != "bd-1" {
		t.Errorf("role calls=%d issueID=%q, want one read for bd-1", role.calls, role.issueID)
	}
}

func TestSourceSnapshotRouteRequiresAuthenticationAndProjectStampBeforeRole(t *testing.T) {
	readiness := readyTransitionReadinessProof(t)
	role := &sourceSnapshotTestReader{snapshot: &issueops.SourceSnapshot{
		Issue: &types.Issue{ID: "bd-1", RowVersion: 1}, OutgoingDependencies: []*types.Dependency{},
	}}
	ts := newTestServer(t, rolesConfig(Config{
		Auth: authTokenFile(t, "token"), SourceSnapshotReader: role,
		TransitionReadiness: readiness, Workspace: domain.ContextInfo{ProjectID: "project-a"},
	}))
	if !ts.sourceSnapshotReady {
		t.Fatal("source snapshot route did not pass its readiness gates")
	}

	response := ts.do(t, http.MethodGet, "/v0/beads/issues/bd-1/sourceSnapshot", nil)
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated source read status = %d, want 401", response.StatusCode)
	}
	response = ts.do(t, http.MethodGet, "/v0/beads/issues/bd-1/sourceSnapshot", bearer("token"))
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("source read without project stamp status = %d, want 400", response.StatusCode)
	}
	wrongProject := bearer("token")
	wrongProject.Set(ProjectIDHeader, "project-b")
	response = ts.do(t, http.MethodGet, "/v0/beads/issues/bd-1/sourceSnapshot", wrongProject)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("source read with mismatched project stamp status = %d, want 400", response.StatusCode)
	}
	if role.calls != 0 {
		t.Fatalf("role calls = %d, want zero before authentication and project checks", role.calls)
	}
}

func TestSourceSnapshotCapabilityRequiresRoleAuthenticationAndProjectIdentity(t *testing.T) {
	readiness := readyTransitionReadinessProof(t)
	for _, tc := range []struct {
		name      string
		withRole  bool
		withAuth  bool
		projectID string
	}{
		{name: "missing role", withAuth: true, projectID: "project-a"},
		{name: "missing authentication", withRole: true, projectID: "project-a"},
		{name: "missing project identity", withRole: true, withAuth: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{TransitionReadiness: readiness, Workspace: domain.ContextInfo{ProjectID: tc.projectID}}
			if tc.withRole {
				cfg.SourceSnapshotReader = &sourceSnapshotTestReader{}
			}
			if tc.withAuth {
				cfg.Auth = authTokenFile(t, "token")
			}
			ts := newTestServer(t, rolesConfig(cfg))
			if ts.sourceSnapshotReady {
				t.Fatal("incomplete source-snapshot gate advertised route readiness")
			}
			contextResponse := ts.do(t, http.MethodGet, "/v0/beads/context", bearer("token"))
			capabilities, ok := decodeBody(t, contextResponse)["capabilities"].([]any)
			if !ok || hasString(capabilities, "issues.sourceSnapshot") {
				t.Fatalf("context advertised source snapshot with incomplete gates: %v", capabilities)
			}
		})
	}
}

func TestSourceSnapshotRouteFailsClosedAndDoesNotReturnPartialRead(t *testing.T) {
	readiness := readyTransitionReadinessProof(t)
	depErr := errors.New("dependency storage failed")
	role := &sourceSnapshotTestReader{err: depErr}
	identity := domain.ContextInfo{ProjectID: "project-a"}
	unready := newTestServer(t, rolesConfig(Config{
		Auth: authTokenFile(t, "token"), SourceSnapshotReader: role,
		Workspace: identity,
	}))
	headers := bearer("token")
	headers.Set(ProjectIDHeader, "project-a")
	contextResponse := unready.do(t, http.MethodGet, "/v0/beads/context", bearer("token"))
	capabilities, ok := decodeBody(t, contextResponse)["capabilities"].([]any)
	if !ok || hasString(capabilities, "issues.sourceSnapshot") {
		t.Fatalf("context advertised source snapshot without readiness proof: %v", capabilities)
	}
	response := unready.do(t, http.MethodGet, "/v0/beads/issues/bd-1/sourceSnapshot", headers)
	if response.StatusCode != http.StatusNotImplemented {
		t.Fatalf("route without readiness proof status = %d, want 501", response.StatusCode)
	}
	if role.calls != 0 {
		t.Fatalf("role calls without readiness proof = %d, want zero", role.calls)
	}

	ts := newTestServer(t, rolesConfig(Config{
		Auth: authTokenFile(t, "token"), SourceSnapshotReader: role,
		TransitionReadiness: readiness, Workspace: identity,
	}))
	if !ts.sourceSnapshotReady {
		t.Fatal("complete readiness gates did not enable source snapshot")
	}
	response = ts.do(t, http.MethodGet, "/v0/beads/issues/bd-1/sourceSnapshot", headers)
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("dependency read failure status = %d, want 500", response.StatusCode)
	}
	if role.calls != 1 {
		t.Fatalf("role calls = %d, want one failed atomic read", role.calls)
	}
}

type stubSourceSnapshotReader struct {
	id    string
	calls int
}

func (s *stubSourceSnapshotReader) ReadSourceSnapshot(_ context.Context, id string) (*issueops.SourceSnapshot, error) {
	s.id, s.calls = id, s.calls+1
	now := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	return &issueops.SourceSnapshot{
		Issue: &types.Issue{ID: id, Title: "source", Priority: 2, Status: types.StatusOpen,
			IssueType: types.TypeTask, CreatedAt: now, UpdatedAt: now, RowVersion: -9223372036854775808},
		OutgoingDependencies: []*types.Dependency{{IssueID: id, DependsOnID: "missing-target", Type: types.DepBlocks}},
	}, nil
}

func TestSourceSnapshotPublishesRevisionAndExactStoredEdges(t *testing.T) {
	reader := &stubSourceSnapshotReader{}
	ts := newTestServer(t, rolesConfig(Config{Auth: authTokenFile(t, "token"), SourceSnapshotReader: reader, TransitionReadiness: readyTransitionReadinessProof(t), Workspace: domain.ContextInfo{ProjectID: "project-a"}}))
	headers := bearer("token")
	headers.Set(ProjectIDHeader, "project-a")
	response := ts.do(t, http.MethodGet, "/v0/beads/issues/bd-source/sourceSnapshot", headers)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.StatusCode, readAll(t, response))
	}
	body := decodeBody(t, response)
	issue, ok := body["issue"].(map[string]any)
	if !ok || issue["id"] != "bd-source" || issue["revision"] != "-9223372036854775808" {
		t.Fatalf("source issue = %#v", body["issue"])
	}
	edges, ok := body["outgoing_dependencies"].([]any)
	if !ok || len(edges) != 1 {
		t.Fatalf("outgoing edges = %#v", body["outgoing_dependencies"])
	}
	edge, ok := edges[0].(map[string]any)
	if !ok || edge["issue_id"] != "bd-source" || edge["depends_on_id"] != "missing-target" || edge["type"] != "blocks" {
		t.Fatalf("stored edge = %#v", edges[0])
	}
	if reader.calls != 1 || reader.id != "bd-source" {
		t.Fatalf("source role calls = %d, id = %q", reader.calls, reader.id)
	}
}

func TestSourceSnapshotUnavailableRoleIsNotAdvertised(t *testing.T) {
	ts := newTestServer(t, rolesConfig(Config{Auth: authTokenFile(t, "token"), TransitionReadiness: readyTransitionReadinessProof(t), Workspace: domain.ContextInfo{ProjectID: "project-a"}}))
	context := decodeBody(t, ts.do(t, http.MethodGet, "/v0/beads/context", bearer("token")))
	for _, capability := range context["capabilities"].([]any) {
		if capability == "issues.sourceSnapshot" {
			t.Fatal("store-backed server advertised a source snapshot role it does not have")
		}
	}
	headers := bearer("token")
	headers.Set(ProjectIDHeader, "project-a")
	response := ts.do(t, http.MethodGet, "/v0/beads/issues/bd-source/sourceSnapshot", headers)
	if response.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501: %s", response.StatusCode, readAll(t, response))
	}
}

func TestSourceSnapshotRejectsMismatchedRoleIdentity(t *testing.T) {
	role := &sourceSnapshotTestReader{snapshot: &issueops.SourceSnapshot{Issue: &types.Issue{ID: "different", RowVersion: 1}}}
	ts := newTestServer(t, rolesConfig(Config{Auth: authTokenFile(t, "token"), SourceSnapshotReader: role,
		TransitionReadiness: readyTransitionReadinessProof(t), Workspace: domain.ContextInfo{ProjectID: "project-a"}}))
	headers := bearer("token")
	headers.Set(ProjectIDHeader, "project-a")
	response := ts.do(t, http.MethodGet, "/v0/beads/issues/requested/sourceSnapshot", headers)
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("mismatched snapshot status=%d body=%s", response.StatusCode, readAll(t, response))
	}
	_ = response.Body.Close()
}
