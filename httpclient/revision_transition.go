// Package httpclient contains importable typed clients for the Beads HTTP API.
package httpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/issueops"
)

const (
	transitionMetadataPath = "/v0/beads/issues/"
	transitionReceiptPath  = "/v0/beads/issue-transition-receipts/"
	projectIDHeader        = "Bd-Project-Id"
	maxProblemBodyBytes    = 1 << 20
	maxSuccessBodyBytes    = 4 << 20
)

// Config supplies the endpoint identity required by revision transition routes.
type Config struct {
	BaseURL    string
	Token      string
	ProjectID  string
	Database   string
	HTTPClient *http.Client
}

// Client is a typed client for the revision-transition routes. Every request
// carries bearer authentication and the mandatory Bd-Project-Id stamp.
type Client struct {
	baseURL   *url.URL
	token     string
	projectID string
	database  string
	http      *http.Client
}

// New constructs a client. The supplied HTTP client's transport and timeout
// are preserved; every request overrides redirect handling and refuses to
// follow redirects so the bearer credential cannot cross endpoint boundaries.
func New(config Config) (*Client, error) {
	base, err := url.Parse(strings.TrimSpace(config.BaseURL))
	if err != nil || base == nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("httpclient: BaseURL must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	if strings.TrimSpace(config.Token) == "" {
		return nil, errors.New("httpclient: bearer token is required")
	}
	if hasControl(config.Token) || !utf8.ValidString(config.Token) {
		return nil, errors.New("httpclient: bearer token contains invalid header text")
	}
	if strings.TrimSpace(config.ProjectID) == "" || hasControl(config.ProjectID) || !utf8.ValidString(config.ProjectID) {
		return nil, errors.New("httpclient: project ID is required")
	}
	if strings.TrimSpace(config.Database) == "" || hasControl(config.Database) || !utf8.ValidString(config.Database) {
		return nil, errors.New("httpclient: database identity is required")
	}
	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{baseURL: base, token: config.Token, projectID: config.ProjectID, database: config.Database, http: httpClient}, nil
}

// TransitionMetadataRequest describes one idempotent revision-fenced metadata
// transition. ReceiptID must remain unchanged when retrying the same request.
// Expected and Value use nil to mean an absent metadata key. A non-nil pointer
// to the raw JSON `null` means the key is present and contains JSON null.
// Payload may be omitted (nil or empty), in which case the server stores `{}`.
type TransitionMetadataRequest struct {
	ReceiptID       string
	Scope           string
	Kind            string
	Actor           string
	ExpectedVersion int64
	Key             string
	Expected        *json.RawMessage
	Value           *json.RawMessage
	Payload         json.RawMessage
}

// TransitionMetadataResult is the answer to one transition attempt. Current
// preserves absent versus present JSON null using a raw message: nil means
// absent, and the bytes `null` mean present JSON null.
type TransitionMetadataResult struct {
	Applied  bool
	Replayed bool
	Current  json.RawMessage
	Receipt  *TransitionReceipt
}

// TransitionReceipt is the immutable record of an applied transition. Marker
// fields preserve the wire distinction between an omitted marker and explicit
// JSON null. ExpectedVersion and ToVersion are parsed from canonical signed
// decimal strings and must fit int64.
type TransitionReceipt struct {
	ReceiptID       string
	IssueID         string
	Scope           string
	Kind            string
	Actor           string
	ExpectedVersion int64
	ToVersion       int64
	Key             string
	Expected        json.RawMessage
	Value           json.RawMessage
	Payload         json.RawMessage
}

// ProblemError carries the stable status and code from an RFC 9457 response.
// Callers should branch on Code and retain the original error for diagnostics.
type ProblemError struct {
	Status int
	Code   string
	Title  string
	Detail string
}

// ServerContext is the authenticated identity snapshot returned by
// GET /v0/beads/context. Capabilities are treated as an allowlist; callers
// should check tokens rather than compare binary versions.
type ServerContext struct {
	APIVersion   string
	ProjectID    string
	Database     string
	Capabilities []string
}

func (e *ProblemError) Error() string {
	if e == nil {
		return "httpclient: <nil> problem"
	}
	if e.Code != "" {
		return fmt.Sprintf("httpclient: Beads returned HTTP %d (%s): %s", e.Status, e.Code, e.Detail)
	}
	return fmt.Sprintf("httpclient: Beads returned HTTP %d: %s", e.Status, e.Detail)
}

// TransitionMetadata posts one transition. An applied response always includes
// a receipt bound to this exact request. If the response is lost, call
// RecoverTransition with the same receipt ID and request before deciding
// whether to issue another transition.
func (c *Client) TransitionMetadata(ctx context.Context, issueID string, request TransitionMetadataRequest) (TransitionMetadataResult, error) {
	if c == nil {
		return TransitionMetadataResult{}, errors.New("httpclient: nil client")
	}
	request.Actor = strings.TrimSpace(request.Actor)
	if err := validateTransitionRequest(issueID, request); err != nil {
		return TransitionMetadataResult{}, err
	}
	if err := c.verifyConfiguredRevisionTransition(ctx); err != nil {
		return TransitionMetadataResult{}, err
	}
	wire := transitionRequestWire{
		ReceiptID:       request.ReceiptID,
		Scope:           request.Scope,
		Kind:            request.Kind,
		Actor:           request.Actor,
		ExpectedVersion: strconv.FormatInt(request.ExpectedVersion, 10),
		Key:             request.Key,
		Expected:        request.Expected,
		Value:           request.Value,
		Payload:         request.Payload,
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return TransitionMetadataResult{}, fmt.Errorf("httpclient: encode transition request: %w", err)
	}
	path := transitionMetadataPath + url.PathEscape(issueID) + ":transitionMetadata"
	response, err := c.do(ctx, http.MethodPost, path, body)
	if err != nil {
		return TransitionMetadataResult{}, err
	}
	var result TransitionMetadataResult
	if err := decodeTransitionResult(response, &result); err != nil {
		return TransitionMetadataResult{}, err
	}
	if result.Receipt != nil {
		if err := ValidateTransitionReceipt(*result.Receipt); err != nil {
			return TransitionMetadataResult{}, fmt.Errorf("httpclient: invalid transition receipt: %w", err)
		}
		plan, err := transitionRequestPlan(issueID, request)
		if err != nil || !storage.SameRevisionTransitionRequest(plan, receiptToIssueops(result.Receipt)) {
			return TransitionMetadataResult{}, errors.New("httpclient: transition receipt does not match the submitted request")
		}
	}
	return result, nil
}

// GetTransitionReceipt fetches an immutable receipt by its exact ID.
func (c *Client) GetTransitionReceipt(ctx context.Context, receiptID string) (*TransitionReceipt, error) {
	if c == nil {
		return nil, errors.New("httpclient: nil client")
	}
	if err := storage.ValidateRevisionTransitionReceiptID(receiptID); err != nil {
		return nil, fmt.Errorf("httpclient: invalid receipt ID: %w", err)
	}
	if err := c.verifyConfiguredRevisionTransition(ctx); err != nil {
		return nil, err
	}
	response, err := c.do(ctx, http.MethodGet, transitionReceiptPath+url.PathEscape(receiptID), nil)
	if err != nil {
		return nil, err
	}
	var receipt TransitionReceipt
	if err := json.Unmarshal(response, &receipt); err != nil {
		return nil, fmt.Errorf("httpclient: decode transition receipt: %w", err)
	}
	if receipt.ReceiptID != receiptID {
		return nil, errors.New("httpclient: receipt response ID does not match the requested ID")
	}
	if err := ValidateTransitionReceipt(receipt); err != nil {
		return nil, fmt.Errorf("httpclient: invalid transition receipt: %w", err)
	}
	return &receipt, nil
}

// GetContext reads the typed identity and capability handshake. It does not
// infer capabilities from the server version.
func (c *Client) GetContext(ctx context.Context) (ServerContext, error) {
	if c == nil {
		return ServerContext{}, errors.New("httpclient: nil client")
	}
	data, err := c.do(ctx, http.MethodGet, "/v0/beads/context", nil)
	if err != nil {
		return ServerContext{}, err
	}
	var wire struct {
		APIVersion   *string  `json:"api_version"`
		ProjectID    *string  `json:"project_id"`
		Database     *string  `json:"database"`
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return ServerContext{}, fmt.Errorf("httpclient: decode context: %w", err)
	}
	if wire.APIVersion == nil || wire.ProjectID == nil || wire.Database == nil || strings.TrimSpace(*wire.Database) == "" || wire.Capabilities == nil {
		return ServerContext{}, errors.New("httpclient: context is missing api_version, project_id, database, or capabilities")
	}
	return ServerContext{APIVersion: *wire.APIVersion, ProjectID: *wire.ProjectID, Database: *wire.Database, Capabilities: append([]string(nil), wire.Capabilities...)}, nil
}

// VerifyRevisionTransition proves that the authenticated endpoint is serving
// the exact configured project and database and advertises both transition
// operations.
// Operations repeat this verification themselves. Call it at controller
// startup when configuration failures should be reported before work begins.
func (c *Client) VerifyRevisionTransition(ctx context.Context, expectedProjectID, expectedDatabase string) error {
	if c == nil {
		return errors.New("httpclient: nil client")
	}
	if strings.TrimSpace(expectedProjectID) == "" {
		return errors.New("httpclient: expected project ID is required")
	}
	if expectedProjectID != c.projectID {
		return fmt.Errorf("httpclient: expected project ID %q does not match the configured request stamp %q", expectedProjectID, c.projectID)
	}
	if strings.TrimSpace(expectedDatabase) == "" {
		return errors.New("httpclient: expected database identity is required")
	}
	if expectedDatabase != c.database {
		return fmt.Errorf("httpclient: expected database %q does not match the configured database %q", expectedDatabase, c.database)
	}
	return c.verifyConfiguredRevisionTransition(ctx)
}

// verifyConfiguredRevisionTransition checks the live endpoint against this
// client's configured identity and the complete capability pair. Mutations
// and receipt reads run this check themselves so callers cannot skip it.
func (c *Client) verifyConfiguredRevisionTransition(ctx context.Context) error {
	server, err := c.GetContext(ctx)
	if err != nil {
		return err
	}
	if server.ProjectID != c.projectID {
		return fmt.Errorf("httpclient: server project ID %q does not match configured project %q", server.ProjectID, c.projectID)
	}
	if server.Database != c.database {
		return fmt.Errorf("httpclient: server database %q does not match configured database %q", server.Database, c.database)
	}
	if server.APIVersion != "v0" {
		return fmt.Errorf("httpclient: unsupported API version %q", server.APIVersion)
	}
	capabilities := make(map[string]struct{}, len(server.Capabilities))
	for _, capability := range server.Capabilities {
		capabilities[capability] = struct{}{}
	}
	for _, required := range []string{"issues.transitionMetadata", "issues.transitionReceipt.get"} {
		if _, ok := capabilities[required]; !ok {
			return fmt.Errorf("httpclient: server does not advertise required capability %q", required)
		}
	}
	return nil
}

// RecoverTransition looks up the stable receipt ID after a lost or ambiguous
// POST response. A found receipt is returned only if it describes the exact
// submitted request. found=false means the server has no receipt yet and the
// caller may retry the original request with the same receipt ID.
func (c *Client) RecoverTransition(ctx context.Context, issueID string, request TransitionMetadataRequest) (*TransitionReceipt, bool, error) {
	request.Actor = strings.TrimSpace(request.Actor)
	if err := validateTransitionRequest(issueID, request); err != nil {
		return nil, false, err
	}
	receipt, err := c.GetTransitionReceipt(ctx, request.ReceiptID)
	if err != nil {
		var problem *ProblemError
		if errors.As(err, &problem) && problem.Status == http.StatusNotFound && problem.Code == "not_found" {
			return nil, false, nil
		}
		return nil, false, err
	}
	plan, err := transitionRequestPlan(issueID, request)
	if err != nil || !storage.SameRevisionTransitionRequest(plan, receiptToIssueops(receipt)) {
		return nil, false, errors.New("httpclient: stored receipt ID is bound to a different transition request")
	}
	return receipt, true, nil
}

// ValidateTransitionReceipt checks the exact data invariants promised by the
// revision-transition receipt route, including signed-int64 revisions and
// absent-versus-null marker validity.
func ValidateTransitionReceipt(receipt TransitionReceipt) error {
	if err := storage.ValidateRevisionTransitionReceiptID(receipt.ReceiptID); err != nil {
		return err
	}
	for _, field := range []struct {
		name  string
		value string
		limit int
	}{
		{"issue ID", receipt.IssueID, 255},
		{"scope", receipt.Scope, 255},
		{"kind", receipt.Kind, 64},
		{"actor", receipt.Actor, 255},
	} {
		if field.value == "" || !utf8.ValidString(field.value) || utf8.RuneCountInString(field.value) > field.limit {
			return fmt.Errorf("%s is empty or exceeds its text bound", field.name)
		}
	}
	if strings.TrimSpace(receipt.Actor) != receipt.Actor || len(receipt.Actor) > 256 || hasControl(receipt.Actor) {
		return errors.New("actor is not a valid canonical actor")
	}
	if receipt.ExpectedVersion == 0 || receipt.ToVersion == 0 || receipt.ToVersion == receipt.ExpectedVersion {
		return errors.New("receipt revisions must be nonzero and distinct")
	}
	if err := storage.ValidateMetadataKey(receipt.Key); err != nil || types.CheckFieldLen("key", receipt.Key) != nil {
		return errors.New("receipt key is invalid")
	}
	expected, err := optionalJSON(receipt.Expected)
	if err != nil {
		return fmt.Errorf("expected marker: %w", err)
	}
	value, err := optionalJSON(receipt.Value)
	if err != nil {
		return fmt.Errorf("value marker: %w", err)
	}
	unchanged, err := storage.MetadataValuesEqual(expected, value)
	if err != nil || unchanged {
		return errors.New("receipt marker transition is invalid")
	}
	if len(receipt.Payload) == 0 || !json.Valid(receipt.Payload) {
		return errors.New("receipt payload is missing or invalid JSON")
	}
	canonical, err := storage.CanonicalMetadataValue(receipt.Payload)
	if err != nil || !bytes.Equal(canonical, receipt.Payload) {
		return errors.New("receipt payload is not canonical JSON")
	}
	return nil
}

func (c *Client) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	endpoint := *c.baseURL
	escapedPath := strings.TrimRight(endpoint.EscapedPath(), "/") + path
	decodedPath, err := url.PathUnescape(escapedPath)
	if err != nil {
		return nil, fmt.Errorf("httpclient: invalid escaped request path: %w", err)
	}
	endpoint.Path = decodedPath
	endpoint.RawPath = escapedPath
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("httpclient: create request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set(projectIDHeader, c.projectID)
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, application/problem+json")
	} else {
		request.Header.Set("Accept", "application/json, application/problem+json")
	}
	requestClient := *c.http
	requestClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	response, err := requestClient.Do(request) //nolint:gosec // G704: host comes from validated client configuration; request IDs only enter escaped paths and redirects are disabled.
	if err != nil {
		return nil, fmt.Errorf("httpclient: %s %s: %w", method, path, err)
	}
	defer func() { _ = response.Body.Close() }()
	maxBodyBytes := maxSuccessBodyBytes
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		maxBodyBytes = maxProblemBodyBytes
	}
	limited := io.LimitReader(response.Body, int64(maxBodyBytes)+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("httpclient: read response: %w", err)
	}
	if len(data) > maxBodyBytes {
		return nil, fmt.Errorf("httpclient: response exceeds %d bytes", maxBodyBytes)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, decodeProblem(response.StatusCode, data)
	}
	if !json.Valid(data) {
		return nil, errors.New("httpclient: server returned invalid JSON")
	}
	return data, nil
}

func decodeProblem(status int, data []byte) error {
	var wire struct {
		Code   string `json:"code"`
		Title  string `json:"title"`
		Detail string `json:"detail"`
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &wire); err != nil {
			return fmt.Errorf("httpclient: Beads returned HTTP %d with an invalid problem document", status)
		}
	}
	return &ProblemError{Status: status, Code: wire.Code, Title: wire.Title, Detail: wire.Detail}
}

type transitionRequestWire struct {
	ReceiptID       string           `json:"receipt_id"`
	Scope           string           `json:"scope"`
	Kind            string           `json:"kind"`
	Actor           string           `json:"actor"`
	ExpectedVersion string           `json:"expected_version"`
	Key             string           `json:"key"`
	Expected        *json.RawMessage `json:"expected,omitempty"`
	Value           *json.RawMessage `json:"value,omitempty"`
	Payload         json.RawMessage  `json:"payload,omitempty"`
}

func decodeTransitionResult(data []byte, result *TransitionMetadataResult) error {
	if err := json.Unmarshal(data, result); err != nil {
		return fmt.Errorf("httpclient: decode transition result: %w", err)
	}
	return nil
}

func validateTransitionRequest(issueID string, request TransitionMetadataRequest) error {
	if issueID == "" || !utf8.ValidString(issueID) || types.CheckFieldLen("id", issueID) != nil || hasControl(issueID) {
		return errors.New("httpclient: issue ID is empty or invalid")
	}
	if !utf8.ValidString(request.Scope) || !utf8.ValidString(request.Kind) || !utf8.ValidString(request.Key) {
		return errors.New("httpclient: transition text fields must be valid UTF-8")
	}
	if request.Actor != strings.TrimSpace(request.Actor) || hasControl(request.Actor) || !utf8.ValidString(request.Actor) || len(request.Actor) > 256 || types.CheckFieldLen("actor", request.Actor) != nil {
		return errors.New("httpclient: actor is not a valid canonical actor")
	}
	if len(request.Payload) > 0 && !json.Valid(request.Payload) {
		return errors.New("httpclient: payload is invalid JSON")
	}
	_, err := transitionRequestPlan(issueID, request)
	if err != nil {
		return fmt.Errorf("httpclient: invalid transition request: %w", err)
	}
	return nil
}

func transitionRequestPlan(issueID string, request TransitionMetadataRequest) (storage.RevisionTransitionPlan, error) {
	return storage.PlanRevisionTransition(issueops.RevisionTransitionRequest{
		ReceiptID:       request.ReceiptID,
		IssueID:         issueID,
		Scope:           request.Scope,
		Kind:            request.Kind,
		Actor:           request.Actor,
		ExpectedVersion: request.ExpectedVersion,
		Key:             request.Key,
		Expected:        request.Expected,
		Value:           request.Value,
		Payload:         request.Payload,
	})
}

func receiptToIssueops(receipt *TransitionReceipt) *issueops.RevisionTransitionReceipt {
	if receipt == nil {
		return nil
	}
	return &issueops.RevisionTransitionReceipt{
		ReceiptID:       receipt.ReceiptID,
		IssueID:         receipt.IssueID,
		Scope:           receipt.Scope,
		Kind:            receipt.Kind,
		Actor:           receipt.Actor,
		ExpectedVersion: receipt.ExpectedVersion,
		ToVersion:       receipt.ToVersion,
		Key:             receipt.Key,
		Expected:        rawPointer(receipt.Expected),
		Value:           rawPointer(receipt.Value),
		Payload:         receipt.Payload,
	}
}

func rawPointer(raw json.RawMessage) *json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	copy := append(json.RawMessage(nil), raw...)
	return &copy
}

func optionalJSON(raw json.RawMessage) (*json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if !json.Valid(raw) {
		return nil, errors.New("invalid JSON value")
	}
	copy := append(json.RawMessage(nil), raw...)
	return &copy, nil
}

func hasControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || r >= 0x7f && r <= 0x9f {
			return true
		}
	}
	return false
}

// UnmarshalJSON validates and decodes a receipt using string revisions. A
// custom decoder is necessary because omitted expected/value markers and
// explicit `null` are separate states in the API.
func (receipt *TransitionReceipt) UnmarshalJSON(data []byte) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return errors.New("transition receipt must be a JSON object")
	}
	allowed := map[string]bool{
		"receipt_id": true, "issue_id": true, "scope": true, "kind": true,
		"actor": true, "expected_version": true, "to_version": true,
		"key": true, "expected": true, "value": true, "payload": true,
	}
	for name := range object {
		if !allowed[name] {
			return fmt.Errorf("transition receipt has unknown member %q", name)
		}
	}
	readString := func(name string) (string, error) {
		raw, ok := object[name]
		if !ok {
			return "", fmt.Errorf("transition receipt is missing %q", name)
		}
		var value *string
		if err := json.Unmarshal(raw, &value); err != nil || value == nil {
			return "", fmt.Errorf("transition receipt %q must be a string", name)
		}
		return *value, nil
	}
	var err error
	if receipt.ReceiptID, err = readString("receipt_id"); err != nil {
		return err
	}
	if receipt.IssueID, err = readString("issue_id"); err != nil {
		return err
	}
	if receipt.Scope, err = readString("scope"); err != nil {
		return err
	}
	if receipt.Kind, err = readString("kind"); err != nil {
		return err
	}
	if receipt.Actor, err = readString("actor"); err != nil {
		return err
	}
	if receipt.Key, err = readString("key"); err != nil {
		return err
	}
	expectedVersion, err := readString("expected_version")
	if err != nil {
		return err
	}
	if receipt.ExpectedVersion, err = parseRevision(expectedVersion); err != nil {
		return fmt.Errorf("expected_version: %w", err)
	}
	toVersion, err := readString("to_version")
	if err != nil {
		return err
	}
	if receipt.ToVersion, err = parseRevision(toVersion); err != nil {
		return fmt.Errorf("to_version: %w", err)
	}
	receipt.Expected = append(receipt.Expected[:0], object["expected"]...)
	receipt.Value = append(receipt.Value[:0], object["value"]...)
	receipt.Payload, err = requiredRaw(object, "payload")
	if err != nil {
		return err
	}
	return nil
}

func parseRevision(token string) (int64, error) {
	value, err := strconv.ParseInt(token, 10, 64)
	if err != nil || value == 0 || types.RevisionToken(value) != token {
		return 0, errors.New("must be a nonzero canonical signed 64-bit decimal string")
	}
	return value, nil
}

func requiredRaw(object map[string]json.RawMessage, name string) (json.RawMessage, error) {
	raw, ok := object[name]
	if !ok || len(raw) == 0 || !json.Valid(raw) {
		return nil, fmt.Errorf("transition receipt %q is missing or invalid JSON", name)
	}
	return append(json.RawMessage(nil), raw...), nil
}

func (result *TransitionMetadataResult) UnmarshalJSON(data []byte) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return errors.New("transition result must be a JSON object")
	}
	allowed := map[string]bool{"applied": true, "replayed": true, "current": true, "receipt": true}
	for name := range object {
		if !allowed[name] {
			return fmt.Errorf("transition result has unknown member %q", name)
		}
	}
	readBool := func(name string) (bool, error) {
		raw, ok := object[name]
		if !ok {
			return false, fmt.Errorf("transition result is missing %q", name)
		}
		var value *bool
		if err := json.Unmarshal(raw, &value); err != nil || value == nil {
			return false, fmt.Errorf("transition result %q must be boolean", name)
		}
		return *value, nil
	}
	var err error
	if result.Applied, err = readBool("applied"); err != nil {
		return err
	}
	if result.Replayed, err = readBool("replayed"); err != nil {
		return err
	}
	result.Current = append(result.Current[:0], object["current"]...)
	if raw, ok := object["current"]; ok && !json.Valid(raw) {
		return errors.New("transition result current is invalid JSON")
	}
	if raw, ok := object["receipt"]; ok {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return errors.New("transition result receipt must be omitted or an object")
		}
		var receipt TransitionReceipt
		if err := json.Unmarshal(raw, &receipt); err != nil {
			return fmt.Errorf("transition result receipt: %w", err)
		}
		result.Receipt = &receipt
	} else {
		result.Receipt = nil
	}
	if result.Replayed && !result.Applied || result.Applied && result.Receipt == nil || !result.Applied && result.Receipt != nil {
		return errors.New("transition result has inconsistent applied, replayed, and receipt fields")
	}
	return nil
}
