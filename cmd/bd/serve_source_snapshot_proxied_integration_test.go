//go:build integration && !windows

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/rhar1511/beads/internal/configfile"
	"github.com/rhar1511/beads/internal/storage/dbproxy/proxy"
)

func TestProxiedServerSourceSnapshot(t *testing.T) {
	// Keep the original PR2 edge/revision assertions, but use the exact
	// candidate and private native Dolt harness so this contract runs without
	// a Docker image or an unrelated embedded build.
	f := newRevisionCLIFixture(t)
	ids := []string{"rvcli-snapshot-source", "rvcli-snapshot-first", "rvcli-snapshot-second"}
	for _, id := range ids {
		f.create(t, id)
	}
	sourceID, firstID, secondID := ids[0], ids[1], ids[2]
	for _, target := range []string{ids[1], ids[2]} {
		if out, err := f.run(t, "dep", "add", ids[0], target); err != 0 {
			t.Fatalf("add edge %s: %v\n%s", target, err, out)
		}
	}
	project, err := configfile.Load(filepath.Join(f.workspace, ".beads"))
	if err != nil || project == nil || project.DoltServerPort == 0 {
		t.Fatalf("project=%+v err=%v", project, err)
	}
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(q43IntegrationToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	proxyRoot := filepath.Join(f.workspace, ".beads", "dolt")
	t.Cleanup(func() {
		if err := proxy.Shutdown(proxyRoot); err != nil {
			t.Errorf("stop snapshot proxy: %v", err)
		}
	})
	q43StartPrivateProxy(t, f.bd, f.workspace, f.env, proxyRoot, project.DoltServerPort)
	serve := q43StartServe(t, f.bd, f.workspace, f.env, tokenFile)
	fetch := func(path string) *http.Response {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, serve.baseURL()+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+q43IntegrationToken)
		request.Header.Set("Bd-Project-Id", project.ProjectID)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	response := fetch("/v0/beads/issues/" + sourceID + "/sourceSnapshot")
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("snapshot status %d: %s", response.StatusCode, raw)
	}
	var got struct {
		Issue struct {
			ID       string `json:"id"`
			Revision string `json:"revision"`
		} `json:"issue"`
		OutgoingDependencies []struct {
			IssueID     string `json:"issue_id"`
			DependsOnID string `json:"depends_on_id"`
			Type        string `json:"type"`
		} `json:"outgoing_dependencies"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Issue.ID != sourceID || got.Issue.Revision == "" || got.Issue.Revision == "0" {
		t.Fatalf("source identity/revision = %q/%q", got.Issue.ID, got.Issue.Revision)
	}
	var targets []string
	for _, edge := range got.OutgoingDependencies {
		if edge.IssueID != sourceID || edge.Type != "blocks" {
			t.Fatalf("unexpected edge: %+v", edge)
		}
		targets = append(targets, edge.DependsOnID)
	}
	slices.Sort(targets)
	want := []string{firstID, secondID}
	slices.Sort(want)
	if !slices.Equal(targets, want) {
		t.Fatalf("outgoing edge targets = %v, want %v", targets, want)
	}
	detailResponse := fetch("/v0/beads/issues/" + sourceID)
	defer detailResponse.Body.Close()
	detail, err := io.ReadAll(detailResponse.Body)
	if err != nil {
		t.Fatal(err)
	}
	status := detailResponse.StatusCode
	var detailIssue struct {
		Revision string `json:"revision"`
	}
	if err := json.Unmarshal(detail, &detailIssue); err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK || detailIssue.Revision != got.Issue.Revision {
		t.Fatalf("source snapshot revision %q disagrees with ordinary read: status %d, body %s", got.Issue.Revision, status, detail)
	}
}
