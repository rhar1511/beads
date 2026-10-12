package httpapi

import (
	"net/http"
	"testing"
)

func TestMetadataTransitionCapabilitiesRequireTransactionalProvider(t *testing.T) {
	for _, cfg := range []Config{
		rolesConfig(Config{}),
		rolesConfig(Config{SourceSnapshotReader: &stubSourceSnapshotReader{}}),
	} {
		ts := newTestServer(t, cfg)
		context := decodeBody(t, ts.get(t, "/v0/beads/context"))
		for _, capability := range context["capabilities"].([]any) {
			if capability == "issues.transitionMetadata" || capability == "issues.transitionReceipt.get" {
				t.Fatalf("store-backed source advertised unsupported transition capability %q", capability)
			}
		}
		response := ts.postBody(t, "/v0/beads/issues/bd-source:transitionMetadata", "application/json", `{
			"receipt_id":"receipt-1","scope":"rig:test","kind":"hold","actor":"test",
			"expected_version":"1","key":"gc.hold","value":"held"
		}`)
		if response.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("transition status = %d, want 503: %s", response.StatusCode, readAll(t, response))
		}
		response = ts.get(t, "/v0/beads/issue-transition-receipts/receipt-1")
		if response.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("receipt lookup status = %d, want 503: %s", response.StatusCode, readAll(t, response))
		}
	}
}
