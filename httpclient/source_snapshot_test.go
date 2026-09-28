package httpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetSourceSnapshotVerifiesIdentityAndReadsTypedSnapshot(t *testing.T) {
	var paths []string
	var gotAuth, gotProject string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.EscapedPath())
		if writeTestContext(w, r, "project-a", "beads-a", []string{"issues.sourceSnapshot", "project.enforce"}) {
			return
		}
		gotAuth = r.Header.Get("Authorization")
		gotProject = r.Header.Get(projectIDHeader)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"issue":{"id":%q,"title":"repair","priority":1,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","revision":"-9223372036854775808"},"outgoing_dependencies":[{"issue_id":%q,"depends_on_id":"bd-base","type":"blocks"}]}`, "bd/one%two?x", "bd/one%two?x")
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL + "/proxy/root/", Token: "secret", ProjectID: "project-a", Database: "beads-a", HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.GetSourceSnapshot(context.Background(), "bd/one%two?x")
	if err != nil {
		t.Fatalf("GetSourceSnapshot: %v", err)
	}
	if snapshot.Issue.ID != "bd/one%two?x" || snapshot.Issue.RowVersion != -1<<63 || snapshot.Issue.Revision != "-9223372036854775808" {
		t.Errorf("issue = %#v, want exact signed-int64 revision and requested ID", snapshot.Issue)
	}
	if len(snapshot.OutgoingDependencies) != 1 || snapshot.OutgoingDependencies[0].IssueID != "bd/one%two?x" || snapshot.OutgoingDependencies[0].DependsOnID != "bd-base" {
		t.Errorf("outgoing dependencies = %#v", snapshot.OutgoingDependencies)
	}
	wantPath := "/proxy/root/v0/beads/issues/bd%2Fone%25two%3Fx/sourceSnapshot"
	if len(paths) != 2 || paths[0] != "/proxy/root/v0/beads/context" || paths[1] != wantPath {
		t.Errorf("request paths = %v, want context then %q", paths, wantPath)
	}
	if gotAuth != "Bearer secret" || gotProject != "project-a" {
		t.Errorf("headers Authorization=%q project=%q", gotAuth, gotProject)
	}
}

func TestGetSourceSnapshotRequiresExactServerIdentityAndCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name         string
		project      string
		database     string
		version      string
		capabilities []string
	}{
		{name: "wrong project", project: "project-b", database: "beads-a", version: "v0", capabilities: []string{"issues.sourceSnapshot", "project.enforce"}},
		{name: "wrong database", project: "project-a", database: "beads-b", version: "v0", capabilities: []string{"issues.sourceSnapshot", "project.enforce"}},
		{name: "unsupported API", project: "project-a", database: "beads-a", version: "v1", capabilities: []string{"issues.sourceSnapshot", "project.enforce"}},
		{name: "missing source capability", project: "project-a", database: "beads-a", version: "v0", capabilities: []string{"project.enforce"}},
		{name: "missing project enforcement", project: "project-a", database: "beads-a", version: "v0", capabilities: []string{"issues.sourceSnapshot"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var snapshotCalls int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v0/beads/context" {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{
						"api_version": tc.version, "project_id": tc.project,
						"database": tc.database, "capabilities": tc.capabilities,
					})
					return
				}
				snapshotCalls++
				_, _ = io.WriteString(w, `{}`)
			}))
			defer server.Close()
			client, err := New(Config{BaseURL: server.URL, Token: "secret", ProjectID: "project-a", Database: "beads-a", HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.GetSourceSnapshot(context.Background(), "bd-1"); err == nil {
				t.Fatal("GetSourceSnapshot accepted an unqualified server")
			}
			if snapshotCalls != 0 {
				t.Errorf("snapshot calls = %d, want none after identity/capability refusal", snapshotCalls)
			}
		})
	}
}

func TestDecodeSourceSnapshotRequiresCanonicalRevisionAndExplicitEdgeArray(t *testing.T) {
	base := `{"issue":{"id":"bd-1","title":"x","priority":1,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","revision":%s},"outgoing_dependencies":%s}`
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "explicit empty", body: fmt.Sprintf(base, `"1"`, `[]`)},
		{name: "missing edge list", body: `{"issue":{"id":"bd-1","revision":"1"}}`},
		{name: "null edge list", body: fmt.Sprintf(base, `"1"`, `null`)},
		{name: "noncanonical revision", body: fmt.Sprintf(base, `"+1"`, `[]`)},
		{name: "zero revision", body: fmt.Sprintf(base, `"0"`, `[]`)},
		{name: "numeric revision", body: fmt.Sprintf(base, `1`, `[]`)},
		{name: "wrong issue", body: strings.ReplaceAll(fmt.Sprintf(base, `"1"`, `[]`), `"bd-1"`, `"bd-other"`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot, err := decodeSourceSnapshot([]byte(tc.body), "bd-1")
			if tc.name == "explicit empty" {
				if err != nil || snapshot == nil || snapshot.OutgoingDependencies == nil || len(snapshot.OutgoingDependencies) != 0 {
					t.Fatalf("decode = (%#v, %v), want explicit empty list", snapshot, err)
				}
				return
			}
			if err == nil || snapshot != nil {
				t.Fatalf("decode = (%#v, %v), want strict refusal", snapshot, err)
			}
		})
	}
}
