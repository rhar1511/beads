package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/rhar1511/beads/internal/storage"
	"github.com/rhar1511/beads/internal/storage/domain"
	storageissueops "github.com/rhar1511/beads/internal/storage/issueops"
	"github.com/rhar1511/beads/issueops"
)

const transitionTestBody = `{"receipt_id":"r-1","scope":"lease","kind":"renew","actor":"  alice  ","expected_version":"9223372036854775807","key":"gc.lease","expected":null,"value":"next"}`

type revisionTransitionTestRole struct {
	mu            sync.Mutex
	requests      []issueops.RevisionTransitionRequest
	results       []issueops.RevisionTransitionResult
	protectedAuth []bool
	getID         string
	receipt       *issueops.RevisionTransitionReceipt
	err           error
	result        issueops.RevisionTransitionResult
	toVersion     *int64
	actorOverride string
}

func (r *revisionTransitionTestRole) TransitionMetadata(ctx context.Context, request issueops.RevisionTransitionRequest) (issueops.RevisionTransitionResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, request)
	r.protectedAuth = append(r.protectedAuth, storageissueops.ProtectedMutationAuthorityConfigured(ctx))
	if r.err != nil {
		return issueops.RevisionTransitionResult{}, r.err
	}
	result := r.result
	if len(r.results) > 0 {
		result = r.results[0]
		r.results = r.results[1:]
	}
	if result.Applied && result.Receipt == nil {
		result.Receipt = transitionTestReceipt(request)
		if r.toVersion != nil {
			result.Receipt.ToVersion = *r.toVersion
		}
		if r.actorOverride != "" {
			result.Receipt.Actor = r.actorOverride
		}
	}
	return result, nil
}

func (r *revisionTransitionTestRole) GetReceipt(_ context.Context, receiptID string) (*issueops.RevisionTransitionReceipt, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.getID = receiptID
	if r.err != nil {
		return nil, r.err
	}
	if r.receipt == nil {
		return nil, storage.ErrNotFound
	}
	return r.receipt, nil
}

func (r *revisionTransitionTestRole) recorded() []issueops.RevisionTransitionRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]issueops.RevisionTransitionRequest(nil), r.requests...)
}

func (r *revisionTransitionTestRole) protectedAuthorityRecorded() []bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]bool(nil), r.protectedAuth...)
}

func transitionBodyWithProtectedPermit(token string) string {
	encoded, _ := json.Marshal(token)
	return strings.TrimSuffix(transitionTestBody, "}") + `,"protected_permit":` + string(encoded) + `}`
}

func transitionTestReceipt(request issueops.RevisionTransitionRequest) *issueops.RevisionTransitionReceipt {
	payload := request.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	return &issueops.RevisionTransitionReceipt{
		ReceiptID: request.ReceiptID, IssueID: request.IssueID, Scope: request.Scope,
		Kind: request.Kind, Actor: request.Actor, ExpectedVersion: request.ExpectedVersion,
		ToVersion: -9223372036854775808, Key: request.Key, Expected: cloneRaw(request.Expected),
		Value: cloneRaw(request.Value), Payload: append(json.RawMessage(nil), payload...),
	}
}

func cloneRaw(raw *json.RawMessage) *json.RawMessage {
	if raw == nil {
		return nil
	}
	copy := append(json.RawMessage(nil), (*raw)...)
	return &copy
}

func TestTransitionRouteRequiresBearerBeforeProjectStamp(t *testing.T) {
	role := &revisionTransitionTestRole{result: issueops.RevisionTransitionResult{Applied: true}}
	ts := newTestServer(t, rolesConfig(Config{
		Auth: authTokenFile(t, "token"), RevisionTransition: role,
		Workspace: domain.ContextInfo{ProjectID: "project-a"},
	}))
	// The startup proof is intentionally absent. Force only the private handler
	// readiness branch so this test isolates the middleware order.
	ts.revisionTransitionReady = true

	resp := ts.do(t, http.MethodPost, "/v0/beads/issues/bd-1:transitionMetadata", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated request status = %d, want 401 before checking the project stamp", resp.StatusCode)
	}

	resp = ts.do(t, http.MethodPost, "/v0/beads/issues/bd-1:transitionMetadata", http.Header{"Authorization": {"Bearer token"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("authenticated request without project stamp status = %d, want 400", resp.StatusCode)
	}
	if body := decodeBody(t, resp); body["param"] != ProjectIDHeader {
		t.Errorf("missing-stamp problem = %v, want param %q", body, ProjectIDHeader)
	}
	if len(role.recorded()) != 0 {
		t.Fatal("role was called before auth/project checks completed")
	}
}

func TestTransitionRouteRejectsMismatchedOrUnknownServerIdentity(t *testing.T) {
	for _, tc := range []struct {
		name     string
		serverID string
		stamp    string
	}{
		{name: "mismatch", serverID: "project-a", stamp: "project-b"},
		{name: "unknown server identity", serverID: "", stamp: "project-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			role := &revisionTransitionTestRole{result: issueops.RevisionTransitionResult{Applied: true}}
			ts := newTestServer(t, rolesConfig(Config{
				Auth: authTokenFile(t, "token"), RevisionTransition: role,
				Workspace: domain.ContextInfo{ProjectID: tc.serverID},
			}))
			ts.revisionTransitionReady = true
			headers := bearer("token")
			headers.Set(ProjectIDHeader, tc.stamp)
			resp := ts.do(t, http.MethodPost, "/v0/beads/issues/bd-1:transitionMetadata", headers)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want project mismatch 400", resp.StatusCode)
			}
			body := decodeBody(t, resp)
			if body["reason"] != string(ReasonProjectMismatch) {
				t.Errorf("reason = %v, want project_mismatch", body["reason"])
			}
			if len(role.recorded()) != 0 {
				t.Fatal("role was called for an untrusted project stamp")
			}
		})
	}
}

func TestTransitionMetadataBindsExactFieldsAndNullMarkers(t *testing.T) {
	role := &revisionTransitionTestRole{result: issueops.RevisionTransitionResult{Applied: true}}
	ts := newTestServer(t, rolesConfig(Config{
		Auth: authTokenFile(t, "token"), RevisionTransition: role,
		Workspace: domain.ContextInfo{ProjectID: "project-a"},
	}))
	ts.revisionTransitionReady = true
	headers := bearer("token")
	headers.Set(ProjectIDHeader, "project-a")
	resp := ts.doBody(t, http.MethodPost, "/v0/beads/issues/bd-1:transitionMetadata", headers, "application/json", transitionTestBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, readAll(t, resp))
	}
	requests := role.recorded()
	if len(requests) != 1 {
		t.Fatalf("role calls = %d, want 1", len(requests))
	}
	request := requests[0]
	if request.IssueID != "bd-1" || request.Actor != "alice" || request.ExpectedVersion != 9223372036854775807 {
		t.Errorf("bound request = %#v", request)
	}
	if request.Expected == nil || string(*request.Expected) != "null" {
		t.Errorf("expected marker = %v, want explicit null", request.Expected)
	}
	if request.Value == nil || string(*request.Value) != `"next"` {
		t.Errorf("value marker = %v, want exact string", request.Value)
	}
	if string(request.Payload) != `{}` {
		t.Errorf("omitted payload = %q, want canonical default", request.Payload)
	}
	if request.ProtectedPermit != "" {
		t.Errorf("ordinary transition permit = %q, want empty", request.ProtectedPermit)
	}
	if got := role.protectedAuthorityRecorded(); len(got) != 1 || got[0] {
		t.Errorf("ordinary transition authority context = %v, want one unconfigured call", got)
	}
	response := decodeBody(t, resp)
	receipt := response["receipt"].(map[string]any)
	if receipt["expected_version"] != "9223372036854775807" || receipt["to_version"] != "-9223372036854775808" {
		t.Errorf("wire revisions = %v, want signed decimal strings", receipt)
	}
}

func TestTransitionMetadataRejectsMalformedProtectedPermit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
	}{
		{name: "null", value: `null`},
		{name: "number", value: `7`},
		{name: "empty", value: `""`},
		{name: "leading whitespace", value: `" permit"`},
		{name: "control character", value: `"permit\u0000suffix"`},
		{name: "line separator", value: `"permit\u2028suffix"`},
		{name: "paragraph separator", value: `"permit\u2029suffix"`},
		{name: "raw invalid UTF-8", value: `"` + string([]byte{0xff}) + `"`},
		{name: "too long", value: `"` + strings.Repeat("x", maxProtectedMutationPermitBytes+1) + `"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			role := &revisionTransitionTestRole{result: issueops.RevisionTransitionResult{Applied: true}}
			ts := newTestServer(t, rolesConfig(Config{
				Auth: authTokenFile(t, "token"), RevisionTransition: role,
				Workspace: domain.ContextInfo{ProjectID: "project-a"},
			}))
			ts.revisionTransitionReady = true
			headers := bearer("token")
			headers.Set(ProjectIDHeader, "project-a")
			body := strings.TrimSuffix(transitionTestBody, "}") + `,"protected_permit":` + tc.value + `}`
			resp := ts.doBody(t, http.MethodPost, "/v0/beads/issues/bd-1:transitionMetadata", headers, "application/json", body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("malformed permit status = %d, want 400: %s", resp.StatusCode, readAll(t, resp))
			}
			if len(role.recorded()) != 0 {
				t.Fatal("malformed permit reached the transition role")
			}
		})
	}
}

func TestTransitionMetadataRejectsUnknownMemberAlongsideProtectedPermit(t *testing.T) {
	role := &revisionTransitionTestRole{result: issueops.RevisionTransitionResult{Applied: true}}
	ts := newTestServer(t, rolesConfig(Config{
		Auth: authTokenFile(t, "token"), RevisionTransition: role,
		Workspace: domain.ContextInfo{ProjectID: "project-a"},
	}))
	ts.revisionTransitionReady = true
	headers := bearer("token")
	headers.Set(ProjectIDHeader, "project-a")
	body := strings.TrimSuffix(transitionBodyWithProtectedPermit("opaque"), "}") + `,"unexpected":true}`
	resp := ts.doBody(t, http.MethodPost, "/v0/beads/issues/bd-1:transitionMetadata", headers, "application/json", body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown-member status = %d, want 400: %s", resp.StatusCode, readAll(t, resp))
	}
	if len(role.recorded()) != 0 {
		t.Fatal("request with an unknown member reached the transition role")
	}
}

func TestTransitionMetadataPreservesAbsentAndNullMarkersAndPayload(t *testing.T) {
	for _, tc := range []struct {
		name         string
		body         string
		wantExpected *string
		wantValue    *string
		wantPayload  string
	}{
		{
			name:        "expected omitted",
			body:        `{"receipt_id":"r-a","scope":"s","kind":"k","actor":"alice","expected_version":"1","key":"gc.k","value":"next"}`,
			wantValue:   strptr(`"next"`),
			wantPayload: `{}`,
		},
		{
			name:         "expected explicit null",
			body:         `{"receipt_id":"r-b","scope":"s","kind":"k","actor":"alice","expected_version":"1","key":"gc.k","expected":null,"value":"next"}`,
			wantExpected: strptr("null"),
			wantValue:    strptr(`"next"`),
			wantPayload:  `{}`,
		},
		{
			name:         "value explicit null and payload null",
			body:         `{"receipt_id":"r-c","scope":"s","kind":"k","actor":"alice","expected_version":"1","key":"gc.k","expected":"before","value":null,"payload":null}`,
			wantExpected: strptr(`"before"`),
			wantValue:    strptr("null"),
			wantPayload:  "null",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			role := &revisionTransitionTestRole{result: issueops.RevisionTransitionResult{Applied: true}}
			ts := newTestServer(t, rolesConfig(Config{
				Auth: authTokenFile(t, "token"), RevisionTransition: role,
				Workspace: domain.ContextInfo{ProjectID: "project-a"},
			}))
			ts.revisionTransitionReady = true
			headers := bearer("token")
			headers.Set(ProjectIDHeader, "project-a")
			resp := ts.doBody(t, http.MethodPost, "/v0/beads/issues/bd-1:transitionMetadata", headers, "application/json", tc.body)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", resp.StatusCode, readAll(t, resp))
			}
			requests := role.recorded()
			if len(requests) != 1 {
				t.Fatalf("role calls = %d, want 1", len(requests))
			}
			assertRawMember(t, "Expected", requests[0].Expected, tc.wantExpected)
			assertRawMember(t, "Value", requests[0].Value, tc.wantValue)
			if got := string(requests[0].Payload); got != tc.wantPayload {
				t.Errorf("Payload = %q, want %q", got, tc.wantPayload)
			}
		})
	}
}

func TestTransitionReplayAndMarkerMissAreSuccessfulAnswers(t *testing.T) {
	null := json.RawMessage("null")
	for _, tc := range []struct {
		name   string
		result issueops.RevisionTransitionResult
	}{
		{name: "replay", result: issueops.RevisionTransitionResult{Applied: true, Replayed: true}},
		{name: "marker miss with null current", result: issueops.RevisionTransitionResult{Current: &null}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			role := &revisionTransitionTestRole{result: tc.result}
			ts := newTestServer(t, rolesConfig(Config{
				Auth: authTokenFile(t, "token"), RevisionTransition: role,
				Workspace: domain.ContextInfo{ProjectID: "project-a"},
			}))
			ts.revisionTransitionReady = true
			headers := bearer("token")
			headers.Set(ProjectIDHeader, "project-a")
			resp := ts.doBody(t, http.MethodPost, "/v0/beads/issues/bd-1:transitionMetadata", headers, "application/json", transitionTestBody)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", resp.StatusCode, readAll(t, resp))
			}
			body := decodeBody(t, resp)
			if tc.result.Replayed && (body["applied"] != true || body["replayed"] != true) {
				t.Errorf("replay body = %v", body)
			}
			if !tc.result.Applied && (body["applied"] != false || body["current"] != nil) {
				t.Errorf("marker-miss body = %v, want applied false and explicit null current", body)
			}
			if !tc.result.Applied {
				if _, present := body["current"]; !present {
					t.Error("marker-miss body omitted the present-null current member")
				}
			}
		})
	}
}

func TestTransitionMetadataRejectsWrongFieldNamesAndRevisionWireValues(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"route ID cannot be rebound in body", strings.Replace(transitionTestBody, `"receipt_id"`, `"issue_id":"other","receipt_id"`, 1)},
		{"numeric revision is refused", strings.Replace(transitionTestBody, `"expected_version":"9223372036854775807"`, `"expected_version":9223372036854775807`, 1)},
		{"overflow is refused", strings.Replace(transitionTestBody, `"expected_version":"9223372036854775807"`, `"expected_version":"9223372036854775808"`, 1)},
		{"underflow is refused", strings.Replace(transitionTestBody, `"expected_version":"9223372036854775807"`, `"expected_version":"-9223372036854775809"`, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			role := &revisionTransitionTestRole{result: issueops.RevisionTransitionResult{Applied: true}}
			ts := newTestServer(t, rolesConfig(Config{
				Auth: authTokenFile(t, "token"), RevisionTransition: role,
				Workspace: domain.ContextInfo{ProjectID: "project-a"},
			}))
			ts.revisionTransitionReady = true
			headers := bearer("token")
			headers.Set(ProjectIDHeader, "project-a")
			resp := ts.doBody(t, http.MethodPost, "/v0/beads/issues/bd-1:transitionMetadata", headers, "application/json", tc.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", resp.StatusCode, readAll(t, resp))
			}
			if len(role.recorded()) != 0 {
				t.Fatal("invalid request reached role")
			}
		})
	}
}

func TestTransitionErrorsHaveStableStatuses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{name: "stale revision", err: storage.ErrVersionMismatch, status: http.StatusConflict, code: string(CodePreconditionFailed)},
		{name: "receipt binding conflict", err: issueops.ErrTransitionReceiptConflict, status: http.StatusConflict, code: string(CodeTransitionReceiptConflict)},
		{
			name:   "protected record refusal",
			err:    fmt.Errorf("permit secret-token for class internal-class: %w", issueops.ErrProtectedRecord),
			status: http.StatusConflict,
			code:   string(CodeProtectedRecord),
		},
		{name: "missing issue", err: storage.ErrNotFound, status: http.StatusNotFound, code: string(CodeNotFound)},
		{name: "wisp unsupported", err: &storage.ErrUnsupported{Op: "revision transition", Backend: "wisp"}, status: http.StatusConflict, code: string(CodeUnsupportedResource)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			role := &revisionTransitionTestRole{err: tc.err}
			ts := newTestServer(t, rolesConfig(Config{
				Auth: authTokenFile(t, "token"), RevisionTransition: role,
				Workspace: domain.ContextInfo{ProjectID: "project-a"},
			}))
			ts.revisionTransitionReady = true
			headers := bearer("token")
			headers.Set(ProjectIDHeader, "project-a")
			resp := ts.doBody(t, http.MethodPost, "/v0/beads/issues/bd-1:transitionMetadata", headers, "application/json", transitionTestBody)
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, tc.status, readAll(t, resp))
			}
			body := decodeBody(t, resp)
			if body["code"] != tc.code {
				t.Errorf("code = %v, want %s", body["code"], tc.code)
			}
			if tc.code == string(CodeProtectedRecord) && body["detail"] != staticDetail[CodeProtectedRecord] {
				t.Errorf("protected refusal detail = %v, want fixed generic detail", body["detail"])
			}
		})
	}
}

func TestTransitionCapabilityRequiresReadinessProof(t *testing.T) {
	role := &revisionTransitionTestRole{}
	ts := newTestServer(t, rolesConfig(Config{
		Auth: authTokenFile(t, "token"), RevisionTransition: role,
		Workspace: domain.ContextInfo{ProjectID: "project-a"},
	}))
	if ts.revisionTransitionReady {
		t.Fatal("zero readiness proof enabled transition routes")
	}
	if got := capabilitiesFor(false); contains(got, "issues.transitionMetadata") || contains(got, "issues.transitionReceipt.get") {
		t.Fatalf("capabilities without readiness include transition tokens: %v", got)
	}
	if got := capabilitiesFor(true); !contains(got, "issues.transitionMetadata") || !contains(got, "issues.transitionReceipt.get") {
		t.Fatalf("capabilities with route readiness omit a transition token: %v", got)
	}
	contextResponse := ts.do(t, http.MethodGet, "/v0/beads/context", bearer("token"))
	contextBody := decodeBody(t, contextResponse)
	contextCaps, ok := contextBody["capabilities"].([]any)
	if !ok || hasAnyString(contextCaps, "issues.transitionMetadata", "issues.transitionReceipt.get") {
		t.Fatalf("context advertises gated transition capabilities without proof: %v", contextBody["capabilities"])
	}
	headers := bearer("token")
	headers.Set(ProjectIDHeader, "project-a")
	resp := ts.doBody(t, http.MethodPost, "/v0/beads/issues/bd-1:transitionMetadata", headers, "application/json", transitionTestBody)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("unready transition status = %d, want stable 501", resp.StatusCode)
	}
	if len(role.recorded()) != 0 {
		t.Fatal("unready transition reached the role")
	}
}

func TestListenKeepsTransitionPairUnavailableWithoutAnyRequiredGate(t *testing.T) {
	for _, tc := range []struct {
		name      string
		withAuth  bool
		withRole  bool
		projectID string
	}{
		{name: "missing auth", withRole: true, projectID: "project-a"},
		{name: "missing role", withAuth: true, projectID: "project-a"},
		{name: "missing project identity", withAuth: true, withRole: true},
		{name: "missing typed readiness proof", withAuth: true, withRole: true, projectID: "project-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{Workspace: domain.ContextInfo{ProjectID: tc.projectID}}
			if tc.withAuth {
				cfg.Auth = authTokenFile(t, "token")
			}
			if tc.withRole {
				cfg.RevisionTransition = &revisionTransitionTestRole{}
			}
			ts := newTestServer(t, rolesConfig(cfg))
			if ts.revisionTransitionReady {
				t.Fatal("Listen enabled transition routes with an incomplete readiness contract")
			}
			contextResponse := ts.do(t, http.MethodGet, "/v0/beads/context", bearer("token"))
			contextBody := decodeBody(t, contextResponse)
			capabilities, ok := contextBody["capabilities"].([]any)
			if !ok || hasAnyString(capabilities, "issues.transitionMetadata", "issues.transitionReceipt.get") {
				t.Fatalf("Listen advertised gated transition capability: %v", contextBody["capabilities"])
			}
		})
	}
}

func TestTransitionMetadataResultRejectsInconsistentRole(t *testing.T) {
	role := &revisionTransitionTestRole{result: issueops.RevisionTransitionResult{Replayed: true}}
	ts := newTestServer(t, rolesConfig(Config{
		Auth: authTokenFile(t, "token"), RevisionTransition: role,
		Workspace: domain.ContextInfo{ProjectID: "project-a"},
	}))
	ts.revisionTransitionReady = true
	headers := bearer("token")
	headers.Set(ProjectIDHeader, "project-a")
	resp := ts.doBody(t, http.MethodPost, "/v0/beads/issues/bd-1:transitionMetadata", headers, "application/json", transitionTestBody)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("inconsistent role status = %d, want 500", resp.StatusCode)
	}
}

func TestTransitionMetadataRejectsInvalidRoleReceipt(t *testing.T) {
	expectedVersion := int64(9223372036854775807)
	for _, tc := range []struct {
		name          string
		toVersion     int64
		actorOverride string
	}{
		{name: "zero resulting revision", toVersion: 0},
		{name: "unchanged resulting revision", toVersion: expectedVersion},
		{name: "request binding differs", toVersion: -9223372036854775808, actorOverride: "mallory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			role := &revisionTransitionTestRole{
				result:        issueops.RevisionTransitionResult{Applied: true},
				toVersion:     &tc.toVersion,
				actorOverride: tc.actorOverride,
			}
			ts := newTestServer(t, rolesConfig(Config{
				Auth: authTokenFile(t, "token"), RevisionTransition: role,
				Workspace: domain.ContextInfo{ProjectID: "project-a"},
			}))
			ts.revisionTransitionReady = true
			headers := bearer("token")
			headers.Set(ProjectIDHeader, "project-a")
			resp := ts.doBody(t, http.MethodPost, "/v0/beads/issues/bd-1:transitionMetadata", headers, "application/json", transitionTestBody)
			if resp.StatusCode != http.StatusInternalServerError {
				t.Fatalf("invalid role receipt status = %d, want 500: %s", resp.StatusCode, readAll(t, resp))
			}
			body := decodeBody(t, resp)
			if _, leaked := body["receipt_id"]; leaked {
				t.Fatalf("invalid role receipt leaked response content: %v", body)
			}
		})
	}
}

func TestTransitionGetReceiptIsValidatedAndUsesExactID(t *testing.T) {
	role := &revisionTransitionTestRole{receipt: &issueops.RevisionTransitionReceipt{
		ReceiptID: "receipt-1", IssueID: "bd-1", Scope: "s", Kind: "k", Actor: "alice",
		ExpectedVersion: -9223372036854775808, ToVersion: 9223372036854775807,
		Key: "gc.k", Value: rawValue(`"v"`), Payload: json.RawMessage(`{}`),
	}}
	ts := newTestServer(t, rolesConfig(Config{
		Auth: authTokenFile(t, "token"), RevisionTransition: role,
		Workspace: domain.ContextInfo{ProjectID: "project-a"},
	}))
	ts.revisionTransitionReady = true
	headers := bearer("token")
	headers.Set(ProjectIDHeader, "project-a")
	resp := ts.do(t, http.MethodGet, "/v0/beads/issue-transition-receipts/receipt-1", headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, readAll(t, resp))
	}
	if role.getID != "receipt-1" {
		t.Errorf("receipt ID passed to role = %q", role.getID)
	}
	body := decodeBody(t, resp)
	if body["expected_version"] != "-9223372036854775808" || body["to_version"] != "9223372036854775807" {
		t.Errorf("wire revisions = %v, want signed decimal strings", body)
	}
}

func TestTransitionGetReceiptRejectsInvalidRoleReceipt(t *testing.T) {
	valid := issueops.RevisionTransitionReceipt{
		ReceiptID: "receipt-1", IssueID: "bd-1", Scope: "s", Kind: "k", Actor: "alice",
		ExpectedVersion: -1, ToVersion: 1,
		Key: "gc.k", Value: rawValue(`"v"`), Payload: json.RawMessage(`{}`),
	}
	for _, tc := range []struct {
		name   string
		mutate func(*issueops.RevisionTransitionReceipt)
	}{
		{name: "different receipt ID", mutate: func(receipt *issueops.RevisionTransitionReceipt) { receipt.ReceiptID = "other-id" }},
		{name: "zero resulting revision", mutate: func(receipt *issueops.RevisionTransitionReceipt) { receipt.ToVersion = 0 }},
		{name: "unchanged resulting revision", mutate: func(receipt *issueops.RevisionTransitionReceipt) { receipt.ToVersion = receipt.ExpectedVersion }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			receipt := valid
			tc.mutate(&receipt)
			role := &revisionTransitionTestRole{receipt: &receipt}
			ts := newTestServer(t, rolesConfig(Config{
				Auth: authTokenFile(t, "token"), RevisionTransition: role,
				Workspace: domain.ContextInfo{ProjectID: "project-a"},
			}))
			ts.revisionTransitionReady = true
			headers := bearer("token")
			headers.Set(ProjectIDHeader, "project-a")
			resp := ts.do(t, http.MethodGet, "/v0/beads/issue-transition-receipts/receipt-1", headers)
			if resp.StatusCode != http.StatusInternalServerError {
				t.Fatalf("invalid role receipt status = %d, want 500: %s", resp.StatusCode, readAll(t, resp))
			}
			body := decodeBody(t, resp)
			if _, leaked := body["receipt_id"]; leaked {
				t.Fatalf("invalid role receipt leaked response content: %v", body)
			}
		})
	}
}

func TestTransitionGetReceiptMissHasReceiptSpecificDetail(t *testing.T) {
	role := &revisionTransitionTestRole{err: storage.ErrNotFound}
	ts := newTestServer(t, rolesConfig(Config{
		Auth: authTokenFile(t, "token"), RevisionTransition: role,
		Workspace: domain.ContextInfo{ProjectID: "project-a"},
	}))
	ts.revisionTransitionReady = true
	headers := bearer("token")
	headers.Set(ProjectIDHeader, "project-a")
	resp := ts.do(t, http.MethodGet, "/v0/beads/issue-transition-receipts/missing", headers)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("receipt miss status = %d, want 404: %s", resp.StatusCode, readAll(t, resp))
	}
	body := decodeBody(t, resp)
	if body["code"] != string(CodeNotFound) || body["detail"] != "no transition receipt exists with that exact ID" {
		t.Fatalf("receipt miss problem = %v, want receipt-specific not_found detail", body)
	}
}

func TestTransitionRouteWithoutServerProjectIdentityFailsClosed(t *testing.T) {
	role := &revisionTransitionTestRole{result: issueops.RevisionTransitionResult{Applied: true}}
	ts := newTestServer(t, rolesConfig(Config{Auth: authTokenFile(t, "token"), RevisionTransition: role}))
	ts.revisionTransitionReady = true
	headers := bearer("token")
	headers.Set(ProjectIDHeader, "project-a")
	resp := ts.doBody(t, http.MethodPost, "/v0/beads/issues/bd-1:transitionMetadata", headers, "application/json", transitionTestBody)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown server identity status = %d, want 400", resp.StatusCode)
	}
	if len(role.recorded()) != 0 {
		t.Fatal("role called while server project identity was unknown")
	}
}

func rawValue(value string) *json.RawMessage {
	raw := json.RawMessage(value)
	return &raw
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func hasAnyString(values []any, wants ...string) bool {
	for _, value := range values {
		text, ok := value.(string)
		if !ok {
			continue
		}
		for _, want := range wants {
			if text == want {
				return true
			}
		}
	}
	return false
}

func (ts *testServer) doBody(t *testing.T, method, path string, headers http.Header, contentType, body string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, ts.base+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header = headers.Clone()
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response, err := ts.client.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	return response
}
