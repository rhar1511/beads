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

	"github.com/rhar1511/beads/issueops"
)

const patchReadyCapabilities = `{"api_version":"v0","project_id":"p-1","database":"beads-a","capabilities":["issues.protectedMutation","issues.transitionPatch","issues.transitionPatchReceipt.get"]}`

func TestTransitionPatchPostsCanonicalRequestAndDecodesNullMarker(t *testing.T) {
	var paths []string
	var gotBody map[string]json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.EscapedPath())
		if strings.HasSuffix(r.URL.Path, "/v0/beads/context") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, patchReadyCapabilities)
			return
		}
		if r.Method != http.MethodPost || r.URL.EscapedPath() != "/proxy/root/v0/beads/issues/bd%2Fone%25two%3Fx:transitionPatch" {
			t.Errorf("patch route = %s %s", r.Method, r.URL.EscapedPath())
		}
		if r.Header.Get("Authorization") != "Bearer token" || r.Header.Get("Bd-Project-Id") != "p-1" {
			t.Errorf("patch headers auth=%q project=%q", r.Header.Get("Authorization"), r.Header.Get("Bd-Project-Id"))
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
			return
		}
		if err := json.Unmarshal(body, &gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"applied":true,"replayed":false,"receipt":{"receipt_id":"patch-1","issue_id":%q,"scope":"q54","kind":"route","actor":"alice","expected_version":"-9223372036854775808","to_version":"9223372036854775807","prior_receipt_id":"parent-1","prior_receipt_digest":"%s","patch":{"metadata":[{"key":"gc.route","expected":null,"value":"ready"}],"status":{"expected":"open","value":"in_progress"},"assignee":{"expected":"","value":"worker-1"},"labels":{"expected":["keep","old"],"value":["keep","new"]}}}}`, "bd/one%two?x", strings.Repeat("a", 64))
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL + "/proxy/root/", Token: "token", ProjectID: "p-1", Database: "beads-a", HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	null := json.RawMessage("null")
	request := TransitionPatchRequest{
		ReceiptID: "patch-1", Scope: "q54", Kind: "route", Actor: " alice ",
		ExpectedVersion: -1 << 63, PriorReceiptID: "parent-1", PriorReceiptDigest: strings.Repeat("a", 64),
		ProtectedPermit: "exact-signed-permit",
		Patch: issueops.RevisionTransitionIssuePatch{
			Metadata: []issueops.RevisionTransitionMetadataPatch{{Key: "gc.route", Expected: &null, Value: patchRaw(`"ready"`)}},
			Status:   &issueops.RevisionTransitionStringPatch{Expected: "open", Value: "in_progress"},
			Assignee: &issueops.RevisionTransitionStringPatch{Expected: "", Value: "worker-1"},
			Labels:   &issueops.RevisionTransitionLabelsPatch{Expected: []string{"old", "keep"}, Value: []string{"new", "keep"}},
		},
	}
	result, err := client.TransitionPatch(context.Background(), "bd/one%two?x", request)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Applied || result.Replayed || result.Receipt == nil {
		t.Fatalf("result = %+v, want applied receipt", result)
	}
	if got, want := paths, []string{"/proxy/root/v0/beads/context", "/proxy/root/v0/beads/issues/bd%2Fone%25two%3Fx:transitionPatch"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("request paths = %v, want %v", got, want)
	}
	if string(gotBody["expected_version"]) != `"-9223372036854775808"` || string(gotBody["protected_permit"]) != `"exact-signed-permit"` {
		t.Fatalf("wire revision/permit = %s / %s", gotBody["expected_version"], gotBody["protected_permit"])
	}
	if _, present := gotBody["issue_id"]; present {
		t.Fatal("wire request duplicated route issue ID")
	}
	var wirePatch map[string]json.RawMessage
	if err := json.Unmarshal(gotBody["patch"], &wirePatch); err != nil {
		t.Fatal(err)
	}
	var metadata []map[string]json.RawMessage
	if err := json.Unmarshal(wirePatch["metadata"], &metadata); err != nil || len(metadata) != 1 || string(metadata[0]["expected"]) != "null" {
		t.Fatalf("metadata wire = %s, err=%v; want explicit null", wirePatch["metadata"], err)
	}
	if got := result.Receipt.Patch.Metadata[0].Expected; got == nil || string(*got) != "null" {
		t.Fatalf("decoded expected marker = %v, want present null", got)
	}
	if got := result.Receipt.Patch.Labels.Expected; fmt.Sprint(got) != "[keep old]" {
		t.Fatalf("receipt labels were not canonicalized: %v", got)
	}
}

func TestTransitionPatchWithholdsWriteWhenCapabilityOrWorkspaceDiffers(t *testing.T) {
	for _, tc := range []struct {
		name         string
		context      string
		wantRequests int
	}{
		{name: "missing protected mutation capability", context: `{"api_version":"v0","project_id":"p-1","database":"beads-a","capabilities":["issues.transitionPatch","issues.transitionPatchReceipt.get"]}`, wantRequests: 1},
		{name: "wrong database", context: `{"api_version":"v0","project_id":"p-1","database":"beads-b","capabilities":["issues.protectedMutation","issues.transitionPatch","issues.transitionPatchReceipt.get"]}`, wantRequests: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var paths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.context)
			}))
			defer server.Close()
			client, err := New(Config{BaseURL: server.URL, Token: "token", ProjectID: "p-1", Database: "beads-a", HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.TransitionPatch(context.Background(), "bd-1", validTransitionPatchRequest())
			if err == nil {
				t.Fatal("TransitionPatch succeeded without the complete configured capability pair")
			}
			if len(paths) != tc.wantRequests || paths[0] != "/v0/beads/context" {
				t.Fatalf("requests = %v, want only context handshake", paths)
			}
		})
	}
}

func TestRecoverTransitionPatchRequiresExactStoredReceipt(t *testing.T) {
	request := validTransitionPatchRequest()
	request.ReceiptID = "patch-recovery"
	var getCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0/beads/context" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, patchReadyCapabilities)
			return
		}
		getCount++
		if r.Method != http.MethodGet || r.URL.EscapedPath() != "/v0/beads/issue-transition-patch-receipts/patch-recovery" {
			t.Errorf("recovery request = %s %s", r.Method, r.URL.EscapedPath())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"receipt_id":"patch-recovery","issue_id":"bd-1","scope":"q54","kind":"route","actor":"alice","expected_version":"1","to_version":"2","prior_receipt_id":"different-parent","prior_receipt_digest":"%s","patch":{"status":{"expected":"open","value":"in_progress"}}}`, strings.Repeat("a", 64))
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, Token: "token", ProjectID: "p-1", Database: "beads-a", HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if receipt, found, err := client.RecoverTransitionPatch(context.Background(), "bd-1", request); err == nil || found || receipt != nil {
		t.Fatalf("RecoverTransitionPatch = (%+v, %v, %v), want exact parent mismatch refusal", receipt, found, err)
	}
	if getCount != 1 {
		t.Fatalf("receipt GET count = %d, want 1", getCount)
	}
}

func validTransitionPatchRequest() TransitionPatchRequest {
	return TransitionPatchRequest{
		ReceiptID: "patch-1", Scope: "q54", Kind: "route", Actor: "alice", ExpectedVersion: 1,
		PriorReceiptID: "parent-1", PriorReceiptDigest: strings.Repeat("a", 64), ProtectedPermit: "permit",
		Patch: issueops.RevisionTransitionIssuePatch{Status: &issueops.RevisionTransitionStringPatch{Expected: "open", Value: "in_progress"}},
	}
}

func patchRaw(value string) *json.RawMessage {
	raw := json.RawMessage(value)
	return &raw
}
