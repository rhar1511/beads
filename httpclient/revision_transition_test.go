package httpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestNewRequiresDatabaseIdentity(t *testing.T) {
	if _, err := New(Config{BaseURL: "http://beads.example", Token: "token", ProjectID: "p-1"}); err == nil {
		t.Fatal("New accepted config without a database identity")
	}
}

func TestTransitionMetadataUsesExactTypedWireAndEscapedBasePath(t *testing.T) {
	var gotPath string
	var gotPaths []string
	var gotAuth, gotProject string
	var gotBody map[string]json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.EscapedPath())
		if writeReadyTestContext(w, r) {
			return
		}
		gotPath = r.URL.EscapedPath()
		gotAuth = r.Header.Get("Authorization")
		gotProject = r.Header.Get("Bd-Project-Id")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		if err := json.Unmarshal(body, &gotBody); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"applied":true,"replayed":false,"current":null,"receipt":{"receipt_id":"stable-receipt","issue_id":%q,"scope":"lease","kind":"renew","actor":"alice","expected_version":"-9223372036854775808","to_version":"9223372036854775807","key":"gc.lease","expected":null,"value":"next","payload":{}}}`, "bd/one%two?x")
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL + "/proxy/root/", Token: "secret", ProjectID: "p-1", Database: "beads-a", HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	null := json.RawMessage("null")
	request := TransitionMetadataRequest{
		ReceiptID:       "stable-receipt",
		Scope:           "lease",
		Kind:            "renew",
		Actor:           " alice ",
		ExpectedVersion: -1 << 63,
		Key:             "gc.lease",
		Expected:        &null,
		Value:           raw(`"next"`),
	}
	result, err := client.TransitionMetadata(context.Background(), "bd/one%two?x", request)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Applied || result.Replayed || result.Receipt == nil {
		t.Fatalf("result = %#v, want applied receipt", result)
	}
	if string(result.Current) != "null" {
		t.Fatalf("current = %q, want explicit null preserved", result.Current)
	}
	if got, want := gotPath, "/proxy/root/v0/beads/issues/bd%2Fone%25two%3Fx:transitionMetadata"; got != want {
		t.Errorf("escaped path = %q, want %q", got, want)
	}
	if len(gotPaths) != 2 || gotPaths[0] != "/proxy/root/v0/beads/context" || gotPaths[1] != gotPath {
		t.Errorf("request order = %v, want context handshake before the transition POST", gotPaths)
	}
	if gotAuth != "Bearer secret" || gotProject != "p-1" {
		t.Errorf("headers Authorization=%q project=%q", gotAuth, gotProject)
	}
	if string(gotBody["expected_version"]) != `"-9223372036854775808"` {
		t.Errorf("expected_version = %s, want signed-int64 decimal string", gotBody["expected_version"])
	}
	if string(gotBody["expected"]) != "null" {
		t.Errorf("expected = %s, want explicit null", gotBody["expected"])
	}
	if string(gotBody["value"]) != `"next"` {
		t.Errorf("value = %s, want exact JSON string", gotBody["value"])
	}
	if _, present := gotBody["payload"]; present {
		t.Errorf("payload = %s, want omitted to preserve server default", gotBody["payload"])
	}
	if _, present := gotBody["issue_id"]; present {
		t.Errorf("request contains issue_id, want route-only binding")
	}
}

func TestTransitionMetadataOmitsAbsentMarkerAndPreservesNullCurrent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeReadyTestContext(w, r) {
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		var members map[string]json.RawMessage
		if err := json.Unmarshal(body, &members); err != nil {
			t.Errorf("decode body: %v", err)
			return
		}
		if _, present := members["expected"]; present {
			t.Errorf("expected member = %s, want omitted for absent marker", members["expected"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"applied":false,"replayed":false,"current":null}`)
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, Token: "secret", ProjectID: "p-1", Database: "beads-a", HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.TransitionMetadata(context.Background(), "bd-1", TransitionMetadataRequest{
		ReceiptID: "r-1", Scope: "s", Kind: "k", Actor: "alice", ExpectedVersion: 1,
		Key: "gc.k", Value: raw(`"next"`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Applied || result.Replayed || result.Receipt != nil || string(result.Current) != "null" {
		t.Fatalf("marker miss = %#v, want false/false/no receipt/present null", result)
	}
}

func TestReceiptWirePreservesAbsentNullAndSignedInt64Range(t *testing.T) {
	var receipt TransitionReceipt
	data := []byte(`{"receipt_id":"r1","issue_id":"bd-1","scope":"s","kind":"k","actor":"a","expected_version":"-9223372036854775808","to_version":"9223372036854775807","key":"gc.k","expected":null,"value":"v","payload":{}}`)
	if err := json.Unmarshal(data, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.ExpectedVersion != -1<<63 || receipt.ToVersion != 1<<63-1 {
		t.Fatalf("versions = %d, %d", receipt.ExpectedVersion, receipt.ToVersion)
	}
	if string(receipt.Expected) != "null" || string(receipt.Value) != `"v"` {
		t.Fatalf("markers expected=%q value=%q", receipt.Expected, receipt.Value)
	}
	if err := ValidateTransitionReceipt(receipt); err != nil {
		t.Fatalf("valid receipt rejected: %v", err)
	}

	var absent TransitionReceipt
	absentData := []byte(`{"receipt_id":"r1","issue_id":"bd-1","scope":"s","kind":"k","actor":"a","expected_version":"-1","to_version":"1","key":"gc.k","value":"v","payload":{}}`)
	if err := json.Unmarshal(absentData, &absent); err != nil {
		t.Fatal(err)
	}
	if absent.Expected != nil {
		t.Fatalf("omitted expected = %q, want nil", absent.Expected)
	}
}

func TestReceiptRejectsNumericOverflowAndUnknownMembers(t *testing.T) {
	valid := `{"receipt_id":"r1","issue_id":"bd-1","scope":"s","kind":"k","actor":"a","expected_version":"-1","to_version":"1","key":"gc.k","value":"v","payload":{}}`
	for name, body := range map[string]string{
		"numeric revision": strings.Replace(valid, `"expected_version":"-1"`, `"expected_version":-1`, 1),
		"overflow":         strings.Replace(valid, `"expected_version":"-1"`, `"expected_version":"9223372036854775808"`, 1),
		"unknown field":    strings.Replace(valid, `"receipt_id":"r1"`, `"receipt_id":"r1","unexpected":true`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			var receipt TransitionReceipt
			if err := json.Unmarshal([]byte(body), &receipt); err == nil {
				t.Fatalf("invalid receipt accepted: %s", body)
			}
		})
	}
}

func TestVerifyRevisionTransitionChecksIdentityAndBothCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name             string
		project          string
		database         string
		expectedDatabase string
		capabilities     []string
		wantErr          bool
	}{
		{name: "ready", project: "p-1", database: "beads-a", expectedDatabase: "beads-a", capabilities: []string{"issues.transitionMetadata", "issues.transitionReceipt.get"}},
		{name: "wrong project", project: "other", database: "beads-a", expectedDatabase: "beads-a", capabilities: []string{"issues.transitionMetadata", "issues.transitionReceipt.get"}, wantErr: true},
		{name: "wrong database", project: "p-1", database: "beads-b", expectedDatabase: "beads-a", capabilities: []string{"issues.transitionMetadata", "issues.transitionReceipt.get"}, wantErr: true},
		{name: "expected database differs from config", project: "p-1", database: "beads-a", expectedDatabase: "beads-b", capabilities: []string{"issues.transitionMetadata", "issues.transitionReceipt.get"}, wantErr: true},
		{name: "receipt route gated", project: "p-1", database: "beads-a", expectedDatabase: "beads-a", capabilities: []string{"issues.transitionMetadata"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v0/beads/context" || r.Header.Get("Authorization") != "Bearer token" {
					t.Errorf("unexpected context request: %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"api_version": "v0", "project_id": tc.project, "database": tc.database, "capabilities": tc.capabilities,
				})
			}))
			defer server.Close()
			client, err := New(Config{BaseURL: server.URL, Token: "token", ProjectID: "p-1", Database: "beads-a", HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			err = client.VerifyRevisionTransition(context.Background(), "p-1", tc.expectedDatabase)
			if (err != nil) != tc.wantErr {
				t.Fatalf("VerifyRevisionTransition error = %v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestTransitionMetadataWithholdsWriteWhenConfiguredDatabaseDiffers(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if writeTestContext(w, r, "p-1", "beads-b", []string{"issues.transitionMetadata", "issues.transitionReceipt.get"}) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"applied":false,"replayed":false}`)
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, Token: "token", ProjectID: "p-1", Database: "beads-a", HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	request := TransitionMetadataRequest{ReceiptID: "r-1", Scope: "s", Kind: "k", Actor: "a", ExpectedVersion: 1, Key: "gc.k", Value: raw(`"v"`)}
	_, err = client.TransitionMetadata(context.Background(), "bd-1", request)
	if err == nil {
		t.Fatal("TransitionMetadata succeeded against a different database")
	}
	if len(paths) != 1 || paths[0] != "/v0/beads/context" {
		t.Fatalf("requests = %v, want only the context handshake and no write", paths)
	}
}

func TestTransitionMetadataWithholdsWriteWhenReceiptCapabilityIsMissing(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if writeTestContext(w, r, "p-1", "beads-a", []string{"issues.transitionMetadata"}) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"applied":false,"replayed":false}`)
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, Token: "token", ProjectID: "p-1", Database: "beads-a", HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	request := TransitionMetadataRequest{ReceiptID: "r-1", Scope: "s", Kind: "k", Actor: "a", ExpectedVersion: 1, Key: "gc.k", Value: raw(`"v"`)}
	if _, err := client.TransitionMetadata(context.Background(), "bd-1", request); err == nil {
		t.Fatal("TransitionMetadata succeeded without the receipt capability")
	}
	if len(paths) != 1 || paths[0] != "/v0/beads/context" {
		t.Fatalf("requests = %v, want only the context handshake and no write", paths)
	}
}

func TestRecoverTransitionWithholdsReceiptReadWhenConfiguredDatabaseDiffers(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if writeTestContext(w, r, "p-1", "beads-b", []string{"issues.transitionMetadata", "issues.transitionReceipt.get"}) {
			return
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"status":404,"code":"not_found","title":"Not Found"}`)
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, Token: "token", ProjectID: "p-1", Database: "beads-a", HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	request := TransitionMetadataRequest{ReceiptID: "r-1", Scope: "s", Kind: "k", Actor: "a", ExpectedVersion: 1, Key: "gc.k", Value: raw(`"v"`)}
	receipt, found, err := client.RecoverTransition(context.Background(), "bd-1", request)
	if err == nil || found || receipt != nil {
		t.Fatalf("RecoverTransition = (%v, %v, %v), want handshake failure", receipt, found, err)
	}
	if len(paths) != 1 || paths[0] != "/v0/beads/context" {
		t.Fatalf("requests = %v, want only the context handshake and no receipt read", paths)
	}
}

func TestRecoverTransitionWithholdsReceiptReadWhenTransitionCapabilityIsMissing(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if writeTestContext(w, r, "p-1", "beads-a", []string{"issues.transitionReceipt.get"}) {
			return
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"status":404,"code":"not_found","title":"Not Found"}`)
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, Token: "token", ProjectID: "p-1", Database: "beads-a", HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	request := TransitionMetadataRequest{ReceiptID: "r-1", Scope: "s", Kind: "k", Actor: "a", ExpectedVersion: 1, Key: "gc.k", Value: raw(`"v"`)}
	receipt, found, err := client.RecoverTransition(context.Background(), "bd-1", request)
	if err == nil || found || receipt != nil {
		t.Fatalf("RecoverTransition = (%v, %v, %v), want handshake failure", receipt, found, err)
	}
	if len(paths) != 1 || paths[0] != "/v0/beads/context" {
		t.Fatalf("requests = %v, want only the context handshake and no receipt read", paths)
	}
}

func TestTransitionMetadataDoesNotFollowRedirects(t *testing.T) {
	var postCount, redirectedCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeReadyTestContext(w, r) {
			return
		}
		if r.URL.Path == "/redirect-target" {
			redirectedCount++
			w.WriteHeader(http.StatusOK)
			return
		}
		postCount++
		http.Redirect(w, r, "/redirect-target", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, Token: "token", ProjectID: "p-1", Database: "beads-a", HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	request := TransitionMetadataRequest{ReceiptID: "r-1", Scope: "s", Kind: "k", Actor: "a", ExpectedVersion: 1, Key: "gc.k", Value: raw(`"v"`)}
	if _, err := client.TransitionMetadata(context.Background(), "bd-1", request); err == nil {
		t.Fatal("TransitionMetadata accepted a redirect")
	}
	if postCount != 1 || redirectedCount != 0 {
		t.Fatalf("requests: POST=%d redirected target=%d, want no redirect follow", postCount, redirectedCount)
	}
}

func TestRecoverTransitionTreatsReceiptMissAsSafeToRetry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeReadyTestContext(w, r) {
			return
		}
		if r.Method != http.MethodGet || r.URL.EscapedPath() != "/v0/beads/issue-transition-receipts/receipt%2F1" {
			t.Errorf("unexpected recovery request: %s %s", r.Method, r.URL.EscapedPath())
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"status":404,"code":"not_found","title":"Not Found"}`)
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, Token: "token", ProjectID: "p-1", Database: "beads-a", HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	request := TransitionMetadataRequest{ReceiptID: "receipt/1", Scope: "s", Kind: "k", Actor: "a", ExpectedVersion: -1, Key: "gc.k", Value: raw(`"v"`)}
	receipt, found, err := client.RecoverTransition(context.Background(), "bd-1", request)
	if err != nil || found || receipt != nil {
		t.Fatalf("RecoverTransition = (%v, %v, %v), want (nil, false, nil)", receipt, found, err)
	}
}

func TestRecoverTransitionAcceptsOnlyAnExactReceipt(t *testing.T) {
	request := TransitionMetadataRequest{
		ReceiptID: "receipt/1", Scope: "scope", Kind: "kind", Actor: "alice",
		ExpectedVersion: -1, Key: "gc.lease", Value: raw(`"value"`),
	}
	for _, tc := range []struct {
		name      string
		actor     string
		payload   string
		wantFound bool
		wantErr   bool
	}{
		{name: "all bound fields match", actor: "alice", payload: `{}`, wantFound: true},
		{name: "actor differs", actor: "bob", payload: `{}`, wantErr: true},
		{name: "payload differs", actor: "alice", payload: `null`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := testReceiptJSON(request, tc.actor, tc.payload)
			if err != nil {
				t.Fatal(err)
			}
			var paths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.EscapedPath())
				if writeReadyTestContext(w, r) {
					return
				}
				if r.URL.EscapedPath() != "/v0/beads/issue-transition-receipts/receipt%2F1" {
					t.Errorf("receipt path = %q", r.URL.EscapedPath())
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(body)
			}))
			defer server.Close()
			client, err := New(Config{BaseURL: server.URL, Token: "token", ProjectID: "p-1", Database: "beads-a", HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			receipt, found, err := client.RecoverTransition(context.Background(), "bd-1", request)
			if (err != nil) != tc.wantErr || found != tc.wantFound {
				t.Fatalf("RecoverTransition = (receipt=%v, found=%v, err=%v), want found=%v err=%v", receipt, found, err, tc.wantFound, tc.wantErr)
			}
			if len(paths) != 2 || paths[0] != "/v0/beads/context" || paths[1] != "/v0/beads/issue-transition-receipts/receipt%2F1" {
				t.Errorf("request order = %v, want context handshake before receipt GET", paths)
			}
			if tc.wantFound && (receipt == nil || receipt.Actor != "alice" || receipt.ReceiptID != request.ReceiptID) {
				t.Fatalf("recovered receipt = %#v", receipt)
			}
		})
	}
}

func TestRecoverTransitionTreatsOnlyExactNotFoundAsSafeAbsence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeReadyTestContext(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"status":404,"code":"unsupported_resource","title":"Not Found"}`)
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, Token: "token", ProjectID: "p-1", Database: "beads-a", HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	request := TransitionMetadataRequest{ReceiptID: "r-1", Scope: "s", Kind: "k", Actor: "a", ExpectedVersion: 1, Key: "gc.k", Value: raw(`"v"`)}
	_, found, err := client.RecoverTransition(context.Background(), "bd-1", request)
	if err == nil || found {
		t.Fatalf("RecoverTransition = (found=%v, err=%v), want non-absence error", found, err)
	}
	problem, ok := err.(*ProblemError)
	if !ok || problem.Status != http.StatusNotFound || problem.Code != "unsupported_resource" {
		t.Fatalf("error = %#v, want typed 404 unsupported_resource", err)
	}
}

func TestRecoverTransitionAcceptsReceiptLargerThanProblemLimit(t *testing.T) {
	payloadText := `{"blob":"` + strings.Repeat("x", maxProblemBodyBytes) + `"}`
	request := TransitionMetadataRequest{
		ReceiptID: "large-receipt", Scope: "s", Kind: "k", Actor: "a",
		ExpectedVersion: 1, Key: "gc.k", Value: raw(`"v"`), Payload: json.RawMessage(payloadText),
	}
	body, err := testReceiptJSON(request, "a", payloadText)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) <= maxProblemBodyBytes {
		t.Fatalf("test receipt is %d bytes, want larger than problem response cap %d", len(body), maxProblemBodyBytes)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeReadyTestContext(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, Token: "token", ProjectID: "p-1", Database: "beads-a", HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	receipt, found, err := client.RecoverTransition(context.Background(), "bd-1", request)
	if err != nil || !found || receipt == nil {
		t.Fatalf("RecoverTransition = (receipt=%v, found=%v, err=%v), want a recovered receipt", receipt, found, err)
	}
	if string(receipt.Payload) != payloadText {
		t.Fatalf("recovered payload length = %d, want %d", len(receipt.Payload), len(payloadText))
	}
}

func TestRecoverTransitionRejectsOversizedProblemResponse(t *testing.T) {
	body := `{"status":404,"code":"not_found","title":"Not Found","detail":"` + strings.Repeat("x", maxProblemBodyBytes) + `"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeReadyTestContext(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, Token: "token", ProjectID: "p-1", Database: "beads-a", HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	request := TransitionMetadataRequest{ReceiptID: "r-1", Scope: "s", Kind: "k", Actor: "a", ExpectedVersion: 1, Key: "gc.k", Value: raw(`"v"`)}
	_, found, err := client.RecoverTransition(context.Background(), "bd-1", request)
	if err == nil || found || !strings.Contains(err.Error(), "response exceeds") {
		t.Fatalf("RecoverTransition = (found=%v, err=%v), want oversized problem error, not safe absence", found, err)
	}
}

func TestTransitionMetadataRejectsReceiptBoundToDifferentRequest(t *testing.T) {
	request := TransitionMetadataRequest{ReceiptID: "r-1", Scope: "s", Kind: "k", Actor: "alice", ExpectedVersion: 1, Key: "gc.k", Value: raw(`"v"`)}
	receipt, err := testReceiptJSON(request, "mallory", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{
		"applied": true, "replayed": true, "receipt": json.RawMessage(receipt),
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeReadyTestContext(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, Token: "token", ProjectID: "p-1", Database: "beads-a", HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.TransitionMetadata(context.Background(), "bd-1", request)
	if err == nil || !strings.Contains(err.Error(), "does not match the submitted request") {
		t.Fatalf("TransitionMetadata error = %v, want receipt binding mismatch", err)
	}
}

func testReceiptJSON(request TransitionMetadataRequest, actor, payload string) ([]byte, error) {
	toVersion := int64(1)
	if request.ExpectedVersion == toVersion {
		toVersion = -1
	}
	receipt := map[string]any{
		"receipt_id":       request.ReceiptID,
		"issue_id":         "bd-1",
		"scope":            request.Scope,
		"kind":             request.Kind,
		"actor":            actor,
		"expected_version": strconv.FormatInt(request.ExpectedVersion, 10),
		"to_version":       strconv.FormatInt(toVersion, 10),
		"key":              request.Key,
		"value":            json.RawMessage(*request.Value),
		"payload":          json.RawMessage(payload),
	}
	if request.Expected != nil {
		receipt["expected"] = json.RawMessage(*request.Expected)
	}
	return json.Marshal(receipt)
}

func raw(value string) *json.RawMessage {
	message := json.RawMessage(value)
	return &message
}

func writeReadyTestContext(w http.ResponseWriter, r *http.Request) bool {
	return writeTestContext(w, r, "p-1", "beads-a", []string{"issues.transitionMetadata", "issues.transitionReceipt.get"})
}

func writeTestContext(w http.ResponseWriter, r *http.Request, projectID, database string, capabilities []string) bool {
	if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/v0/beads/context") {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"api_version":  "v0",
		"project_id":   projectID,
		"database":     database,
		"capabilities": capabilities,
	})
	return true
}
