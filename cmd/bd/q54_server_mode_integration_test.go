//go:build integration && !windows

package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/steveyegge/beads/httpclient"
	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/protectedmutation"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/dbproxy/proxy"
	publicops "github.com/steveyegge/beads/issueops"
)

// This process-boundary test uses the exact candidate, an owned SQL proxy and
// private Dolt server, and real Ed25519 permits. It covers the served UOW path,
// where a policy decorator can hide readiness or the optional patch use case.
// It does not qualify the controller's separate host key-broker deployment.
func TestQ54ServerModeSignedPatchConformance(t *testing.T) {
	f := newRevisionCLIFixture(t)
	project, err := configfile.Load(filepath.Join(f.workspace, ".beads"))
	if err != nil || project == nil || project.DoltServerPort == 0 {
		t.Fatalf("private project configuration: %+v, %v", project, err)
	}
	id := "rvcli-signed-patch"
	f.create(t, id)
	before := f.row(t, id)
	directory := t.TempDir()
	authorityPath, private := writeTestProtectedMutationAuthorityAndKey(t, directory, project.ProjectID, project.DoltDatabase)
	env := q43SetEnv(f.env, serveProtectedMutationAuthorityEnv+"="+authorityPath)
	tokenFile := filepath.Join(directory, "serve-token")
	if err := os.WriteFile(tokenFile, []byte(q43IntegrationToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	proxyRoot := filepath.Join(f.workspace, ".beads", "dolt")
	t.Cleanup(func() {
		if err := proxy.Shutdown(proxyRoot); err != nil {
			t.Errorf("stop private Q54 proxy: %v", err)
		}
	})
	q43StartPrivateProxy(t, f.bd, f.workspace, f.env, proxyRoot, project.DoltServerPort)
	serve := q43StartServe(t, f.bd, f.workspace, env, tokenFile)
	client := q43Client(t, serve.baseURL(), project.ProjectID, project.DoltDatabase, http.DefaultClient)
	if err := client.VerifyRevisionTransitionPatch(t.Context(), project.ProjectID, project.DoltDatabase); err != nil {
		t.Fatalf("signed patch capability: %v", err)
	}
	initial := q43TransitionRequest("q54-native-parent", "admitted", q43IssueRevision(t, serve.baseURL(), project.ProjectID, id))
	parent, err := client.TransitionMetadata(t.Context(), id, initial)
	if err != nil || parent.Receipt == nil || !parent.Applied {
		t.Fatalf("parent transition: %+v, %v", parent, err)
	}
	r := parent.Receipt
	digest, err := publicops.RevisionTransitionReceiptDigest(publicops.RevisionTransitionReceipt{
		ReceiptID: r.ReceiptID, IssueID: r.IssueID, Scope: r.Scope, Kind: r.Kind, Actor: r.Actor,
		ExpectedVersion: r.ExpectedVersion, ToVersion: r.ToVersion, Key: r.Key,
		Expected: initial.Expected, Value: initial.Value, Payload: r.Payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	next := json.RawMessage(`"routed"`)
	request := httpclient.TransitionPatchRequest{
		ReceiptID: "q54-native-patch", Scope: initial.Scope, Kind: "route", Actor: initial.Actor,
		ExpectedVersion: r.ToVersion, PriorReceiptID: r.ReceiptID, PriorReceiptDigest: digest,
		Patch: publicops.RevisionTransitionIssuePatch{
			Metadata: []publicops.RevisionTransitionMetadataPatch{{Key: initial.Key, Expected: initial.Value, Value: &next}},
			Status:   &publicops.RevisionTransitionStringPatch{Expected: "open", Value: "in_progress"},
			Assignee: &publicops.RevisionTransitionStringPatch{Expected: "", Value: "native-worker"},
			Labels:   &publicops.RevisionTransitionLabelsPatch{Expected: []string{}, Value: []string{"admitted"}},
		},
	}
	request.ProtectedPermit = q54NativePermit(t, private, project, id, request, "q54-native-use-0001")
	bad := request
	bad.ProtectedPermit = "invalid.signed-permit"
	if _, err := client.TransitionPatch(t.Context(), id, bad); err == nil {
		t.Fatal("invalid permit applied a patch")
	}
	if got := q43IssueRevision(t, serve.baseURL(), project.ProjectID, id); got != r.ToVersion {
		t.Fatalf("rejected permit changed revision: %d", got)
	}
	result, err := client.TransitionPatch(t.Context(), id, request)
	if err != nil || !result.Applied || result.Replayed || result.Receipt == nil {
		t.Fatalf("signed patch: %+v, %v", result, err)
	}
	after := f.row(t, id)
	if after.Status != "in_progress" || after.Assignee != "native-worker" || after.Title != before.Title {
		t.Fatalf("atomic source update: %+v", after)
	}
	snapshot, err := client.GetSourceSnapshot(t.Context(), id)
	if err != nil || snapshot == nil || !reflect.DeepEqual(snapshot.Issue.Labels, []string{"admitted"}) {
		t.Fatalf("patched source snapshot: %+v, %v", snapshot, err)
	}
	var marker string
	if err := f.db.QueryRowContext(t.Context(), "SELECT JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.q43_transition_state')) FROM issues WHERE id = ?", id).Scan(&marker); err != nil || marker != "routed" {
		t.Fatalf("patched metadata=%q, %v", marker, err)
	}
	if err := serve.stop(); err != nil {
		t.Fatal(err)
	}
	serve = q43StartServe(t, f.bd, f.workspace, env, tokenFile)
	client = q43Client(t, serve.baseURL(), project.ProjectID, project.DoltDatabase, http.DefaultClient)
	// Exact replay is established from durable evidence, even when the caller
	// no longer supplies a valid original permit. It must consume no new use.
	replay, err := client.TransitionPatch(t.Context(), id, bad)
	if err != nil || !replay.Replayed || replay.Receipt == nil || replay.Receipt.ToVersion != result.Receipt.ToVersion {
		t.Fatalf("restart replay: %+v, %v", replay, err)
	}
	f.unchanged(t, id, after)
	for _, table := range []string{"protected_mutation_permit_uses", "issue_transition_receipts"} {
		var dirty int
		if err := f.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM dolt_status WHERE table_name = ?", table).Scan(&dirty); err != nil || dirty != 0 {
			t.Fatalf("%s publication: dirty=%d, %v", table, dirty, err)
		}
	}
	var uses int
	if err := f.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM protected_mutation_permit_uses WHERE replay_id = ?", "q54-native-use-0001").Scan(&uses); err != nil || uses != 1 {
		t.Fatalf("permit consumption after replay: %d, %v", uses, err)
	}
}

func q54NativePermit(t *testing.T, key ed25519.PrivateKey, project *configfile.Config, id string, request httpclient.TransitionPatchRequest, replayID string) string {
	t.Helper()
	plan, err := storage.PlanRevisionTransitionPatch(publicops.RevisionTransitionPatchRequest{
		ReceiptID: request.ReceiptID, IssueID: id, Scope: request.Scope, Kind: request.Kind, Actor: request.Actor,
		ExpectedVersion: request.ExpectedVersion, PriorReceiptID: request.PriorReceiptID,
		PriorReceiptDigest: request.PriorReceiptDigest, Patch: request.Patch,
	})
	if err != nil {
		t.Fatal(err)
	}
	digest, err := publicops.ProtectedMutationDigest(plan)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	data, err := protectedmutation.GrantSigningBytes(protectedmutation.Claims{
		SchemaVersion: protectedmutation.SchemaVersionV1, Purpose: protectedmutation.PermitKeyPurpose,
		KeyID: "permit-key", Issuer: "issuer-a", Audience: "audience-a",
		ProjectID: project.ProjectID, Database: project.DoltDatabase,
		Operation: "issue.revision_transition_patch", ResourceIDs: []string{id}, RequestDigest: digest,
		ReplayID: replayID, IssuedAt: now.Format(time.RFC3339Nano), ExpiresAt: now.Add(4 * time.Minute).Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(data[len(protectedmutation.PermitSigningDomain):]) + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, data))
}
