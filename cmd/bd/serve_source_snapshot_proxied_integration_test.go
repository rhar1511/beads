//go:build cgo

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"testing"
)

func TestProxiedServerSourceSnapshot(t *testing.T) {
	requireSharedProxiedServer(t)
	bd := buildEmbeddedBD(t)
	p := newSharedProxiedProject(t, bd, "srvsnapshot")
	source := bdProxiedCreate(t, bd, p.dir, "snapshot source")
	first := bdProxiedCreate(t, bd, p.dir, "first target")
	second := bdProxiedCreate(t, bd, p.dir, "second target")
	for _, target := range []string{first.ID, second.ID} {
		if out, err := bdProxiedRun(t, bd, p.dir, "dep", "add", source.ID, target); err != nil {
			t.Fatalf("add edge %s: %v\n%s", target, err, out)
		}
	}
	sp := startServe(t, bd, p.dir, bdProxiedEnv(p.dir))
	response, err := sp.client.Get(sp.url("/v0/beads/issues/" + source.ID + "/sourceSnapshot"))
	if err != nil {
		t.Fatal(err)
	}
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
	if got.Issue.ID != source.ID || got.Issue.Revision == "" || got.Issue.Revision == "0" {
		t.Fatalf("source identity/revision = %q/%q", got.Issue.ID, got.Issue.Revision)
	}
	var targets []string
	for _, edge := range got.OutgoingDependencies {
		if edge.IssueID != source.ID || edge.Type != "blocks" {
			t.Fatalf("unexpected edge: %+v", edge)
		}
		targets = append(targets, edge.DependsOnID)
	}
	slices.Sort(targets)
	want := []string{first.ID, second.ID}
	slices.Sort(want)
	if !slices.Equal(targets, want) {
		t.Fatalf("outgoing edge targets = %v, want %v", targets, want)
	}
	status, detail := sp.getIssueRaw(t, source.ID)
	if status != http.StatusOK || revisionOf(t, detail) != got.Issue.Revision {
		t.Fatalf("source snapshot revision %q disagrees with ordinary read: status %d, body %s", got.Issue.Revision, status, detail)
	}
}
