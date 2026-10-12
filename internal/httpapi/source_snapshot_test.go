package httpapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/issueops"
)

type stubSourceSnapshotReader struct {
	id    string
	calls int
}

func (s *stubSourceSnapshotReader) ReadSourceSnapshot(_ context.Context, id string) (issueops.SourceSnapshot, error) {
	s.id, s.calls = id, s.calls+1
	now := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	return issueops.SourceSnapshot{
		Issue: &types.Issue{ID: id, Title: "source", Priority: 2, Status: types.StatusOpen,
			IssueType: types.TypeTask, CreatedAt: now, UpdatedAt: now, RowVersion: -9223372036854775808},
		OutgoingDependencies: []*types.Dependency{{IssueID: id, DependsOnID: "missing-target", Type: types.DepBlocks}},
	}, nil
}

func TestSourceSnapshotPublishesRevisionAndExactStoredEdges(t *testing.T) {
	reader := &stubSourceSnapshotReader{}
	ts := newTestServer(t, rolesConfig(Config{SourceSnapshotReader: reader}))
	response := ts.get(t, "/v0/beads/issues/bd-source/sourceSnapshot")
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
	ts := newTestServer(t, rolesConfig(Config{}))
	context := decodeBody(t, ts.get(t, "/v0/beads/context"))
	for _, capability := range context["capabilities"].([]any) {
		if capability == "issues.sourceSnapshot" {
			t.Fatal("store-backed server advertised a source snapshot role it does not have")
		}
	}
	response := ts.get(t, "/v0/beads/issues/bd-source/sourceSnapshot")
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", response.StatusCode, readAll(t, response))
	}
}
