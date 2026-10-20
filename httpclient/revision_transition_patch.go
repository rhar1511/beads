package httpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/rhar1511/beads/internal/storage"
	"github.com/rhar1511/beads/internal/types"
	"github.com/rhar1511/beads/issueops"
)

const transitionPatchReceiptPath = "/v0/beads/issue-transition-patch-receipts/"

// TransitionPatchRequest describes one atomic source patch. ReceiptID must
// remain fixed for retries, and ProtectedPermit must be the signed token for
// this exact canonical operation and workspace.
type TransitionPatchRequest struct {
	ReceiptID          string
	Scope              string
	Kind               string
	Actor              string
	ExpectedVersion    int64
	PriorReceiptID     string
	PriorReceiptDigest string
	Patch              issueops.RevisionTransitionIssuePatch
	ProtectedPermit    string
}

// TransitionPatchReceipt is the immutable backend-produced source revision
// transition and its exact parent-receipt link.
type TransitionPatchReceipt struct {
	ReceiptID          string
	IssueID            string
	Scope              string
	Kind               string
	Actor              string
	ExpectedVersion    int64
	ToVersion          int64
	PriorReceiptID     string
	PriorReceiptDigest string
	Patch              issueops.RevisionTransitionIssuePatch
}

// TransitionPatchResult is a successful applied patch or exact replay.
type TransitionPatchResult struct {
	Applied  bool
	Replayed bool
	Receipt  *TransitionPatchReceipt
}

// TransitionPatch submits an exact patch only after the endpoint identity and
// the entire patch/protected-mutation capability pair are verified.
func (c *Client) TransitionPatch(ctx context.Context, issueID string, request TransitionPatchRequest) (TransitionPatchResult, error) {
	if c == nil {
		return TransitionPatchResult{}, errors.New("httpclient: nil client")
	}
	request.Actor = strings.TrimSpace(request.Actor)
	plan, err := transitionPatchRequestPlan(issueID, request)
	if err != nil {
		return TransitionPatchResult{}, err
	}
	if err := c.verifyConfiguredRevisionTransitionPatch(ctx); err != nil {
		return TransitionPatchResult{}, err
	}
	wire := transitionPatchRequestWire{
		ReceiptID: plan.ReceiptID, Scope: plan.Scope, Kind: plan.Kind, Actor: plan.Actor,
		ExpectedVersion: types.RevisionToken(plan.ExpectedVersion),
		PriorReceiptID:  plan.PriorReceiptID, PriorReceiptDigest: plan.PriorReceiptDigest,
		Patch: plan.Patch, ProtectedPermit: request.ProtectedPermit,
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return TransitionPatchResult{}, fmt.Errorf("httpclient: encode transition patch request: %w", err)
	}
	response, err := c.do(ctx, http.MethodPost, transitionMetadataPath+url.PathEscape(issueID)+":transitionPatch", body)
	if err != nil {
		return TransitionPatchResult{}, err
	}
	var result TransitionPatchResult
	if err := json.Unmarshal(response, &result); err != nil {
		return TransitionPatchResult{}, fmt.Errorf("httpclient: decode transition patch result: %w", err)
	}
	if !result.Applied || result.Receipt == nil || result.Replayed && !result.Applied {
		return TransitionPatchResult{}, errors.New("httpclient: transition patch result is incomplete")
	}
	if err := ValidateTransitionPatchReceipt(*result.Receipt); err != nil {
		return TransitionPatchResult{}, fmt.Errorf("httpclient: invalid transition patch receipt: %w", err)
	}
	if !sameTransitionPatchRequest(plan, result.Receipt) {
		return TransitionPatchResult{}, errors.New("httpclient: transition patch receipt does not match the submitted request")
	}
	return result, nil
}

// GetTransitionPatchReceipt reads an exact durable patch receipt by ID.
func (c *Client) GetTransitionPatchReceipt(ctx context.Context, receiptID string) (*TransitionPatchReceipt, error) {
	if c == nil {
		return nil, errors.New("httpclient: nil client")
	}
	if err := storage.ValidateRevisionTransitionReceiptID(receiptID); err != nil {
		return nil, fmt.Errorf("httpclient: invalid patch receipt ID: %w", err)
	}
	if err := c.verifyConfiguredRevisionTransitionPatch(ctx); err != nil {
		return nil, err
	}
	data, err := c.do(ctx, http.MethodGet, transitionPatchReceiptPath+url.PathEscape(receiptID), nil)
	if err != nil {
		return nil, err
	}
	var receipt TransitionPatchReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		return nil, fmt.Errorf("httpclient: decode transition patch receipt: %w", err)
	}
	if receipt.ReceiptID != receiptID {
		return nil, errors.New("httpclient: patch receipt response ID does not match the requested ID")
	}
	if err := ValidateTransitionPatchReceipt(receipt); err != nil {
		return nil, fmt.Errorf("httpclient: invalid transition patch receipt: %w", err)
	}
	return &receipt, nil
}

// RecoverTransitionPatch returns the stored receipt only if it is bound to the
// exact submitted request. A missing receipt permits retrying that exact body
// with the same ID; it does not choose another action.
func (c *Client) RecoverTransitionPatch(ctx context.Context, issueID string, request TransitionPatchRequest) (*TransitionPatchReceipt, bool, error) {
	if c == nil {
		return nil, false, errors.New("httpclient: nil client")
	}
	request.Actor = strings.TrimSpace(request.Actor)
	plan, err := transitionPatchRequestPlan(issueID, request)
	if err != nil {
		return nil, false, err
	}
	receipt, err := c.GetTransitionPatchReceipt(ctx, request.ReceiptID)
	if err != nil {
		var problem *ProblemError
		if errors.As(err, &problem) && problem.Status == http.StatusNotFound && problem.Code == "not_found" {
			return nil, false, nil
		}
		return nil, false, err
	}
	if !sameTransitionPatchRequest(plan, receipt) {
		return nil, false, errors.New("httpclient: stored patch receipt ID is bound to a different request")
	}
	return receipt, true, nil
}

// VerifyRevisionTransitionPatch checks workspace identity and requires both
// patch routes plus startup protected-mutation readiness.
func (c *Client) VerifyRevisionTransitionPatch(ctx context.Context, expectedProjectID, expectedDatabase string) error {
	if c == nil {
		return errors.New("httpclient: nil client")
	}
	if expectedProjectID != c.projectID {
		return fmt.Errorf("httpclient: expected project ID %q does not match the configured request stamp %q", expectedProjectID, c.projectID)
	}
	if strings.TrimSpace(expectedDatabase) == "" || expectedDatabase != c.database {
		return errors.New("httpclient: expected database does not match the configured database")
	}
	return c.verifyConfiguredRevisionTransitionPatch(ctx)
}

func (c *Client) verifyConfiguredRevisionTransitionPatch(ctx context.Context) error {
	server, err := c.GetContext(ctx)
	if err != nil {
		return err
	}
	if server.ProjectID != c.projectID || server.Database != c.database {
		return errors.New("httpclient: server workspace identity does not match the configured project and database")
	}
	if server.APIVersion != "v0" {
		return fmt.Errorf("httpclient: unsupported API version %q", server.APIVersion)
	}
	capabilities := make(map[string]struct{}, len(server.Capabilities))
	for _, capability := range server.Capabilities {
		capabilities[capability] = struct{}{}
	}
	for _, required := range []string{"issues.transitionPatch", "issues.transitionPatchReceipt.get", "issues.protectedMutation"} {
		if _, ok := capabilities[required]; !ok {
			return fmt.Errorf("httpclient: server does not advertise required capability %q", required)
		}
	}
	return nil
}

func transitionPatchRequestPlan(issueID string, request TransitionPatchRequest) (storage.RevisionTransitionPatchPlan, error) {
	if issueID == "" || !utf8.ValidString(issueID) || types.CheckFieldLen("id", issueID) != nil || hasControl(issueID) {
		return storage.RevisionTransitionPatchPlan{}, errors.New("httpclient: issue ID is empty or invalid")
	}
	if request.Actor != strings.TrimSpace(request.Actor) || hasControl(request.Actor) || !utf8.ValidString(request.Actor) || len(request.Actor) > 256 || types.CheckFieldLen("actor", request.Actor) != nil {
		return storage.RevisionTransitionPatchPlan{}, errors.New("httpclient: actor is not a valid canonical actor")
	}
	if request.ProtectedPermit == "" || strings.TrimSpace(request.ProtectedPermit) != request.ProtectedPermit ||
		!utf8.ValidString(request.ProtectedPermit) || hasPatchPermitControl(request.ProtectedPermit) || len(request.ProtectedPermit) > 65536 {
		return storage.RevisionTransitionPatchPlan{}, errors.New("httpclient: protected permit is empty or invalid")
	}
	return storage.PlanRevisionTransitionPatch(issueops.RevisionTransitionPatchRequest{
		ReceiptID: request.ReceiptID, IssueID: issueID, Scope: request.Scope, Kind: request.Kind,
		Actor: request.Actor, ExpectedVersion: request.ExpectedVersion,
		PriorReceiptID: request.PriorReceiptID, PriorReceiptDigest: request.PriorReceiptDigest,
		Patch: request.Patch,
	})
}

func hasPatchPermitControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) || r == '\u2028' || r == '\u2029' {
			return true
		}
	}
	return false
}

func sameTransitionPatchRequest(plan storage.RevisionTransitionPatchPlan, receipt *TransitionPatchReceipt) bool {
	if receipt == nil {
		return false
	}
	return storage.SameRevisionTransitionPatchRequest(plan, &issueops.RevisionTransitionPatchReceipt{
		ReceiptID: receipt.ReceiptID, IssueID: receipt.IssueID, Scope: receipt.Scope,
		Kind: receipt.Kind, Actor: receipt.Actor, ExpectedVersion: receipt.ExpectedVersion,
		ToVersion: receipt.ToVersion, PriorReceiptID: receipt.PriorReceiptID,
		PriorReceiptDigest: receipt.PriorReceiptDigest, Patch: receipt.Patch,
	})
}

// ValidateTransitionPatchReceipt checks exact revisions, canonical patch
// shape, and parent receipt linkage before the client uses a receipt.
func ValidateTransitionPatchReceipt(receipt TransitionPatchReceipt) error {
	if err := storage.ValidateRevisionTransitionReceiptID(receipt.ReceiptID); err != nil {
		return err
	}
	if receipt.ToVersion == 0 || receipt.ToVersion == receipt.ExpectedVersion {
		return errors.New("patch receipt revisions are zero or unchanged")
	}
	plan, err := storage.PlanRevisionTransitionPatch(issueops.RevisionTransitionPatchRequest{
		ReceiptID: receipt.ReceiptID, IssueID: receipt.IssueID, Scope: receipt.Scope,
		Kind: receipt.Kind, Actor: receipt.Actor, ExpectedVersion: receipt.ExpectedVersion,
		PriorReceiptID: receipt.PriorReceiptID, PriorReceiptDigest: receipt.PriorReceiptDigest,
		Patch: receipt.Patch,
	})
	if err != nil {
		return err
	}
	if !storage.SameRevisionTransitionPatchRequest(plan, &issueops.RevisionTransitionPatchReceipt{
		ReceiptID: receipt.ReceiptID, IssueID: receipt.IssueID, Scope: receipt.Scope,
		Kind: receipt.Kind, Actor: receipt.Actor, ExpectedVersion: receipt.ExpectedVersion,
		ToVersion: receipt.ToVersion, PriorReceiptID: receipt.PriorReceiptID,
		PriorReceiptDigest: receipt.PriorReceiptDigest, Patch: receipt.Patch,
	}) {
		return errors.New("patch receipt is not canonically bound to its request")
	}
	return nil
}

type transitionPatchRequestWire struct {
	ReceiptID          string                                `json:"receipt_id"`
	Scope              string                                `json:"scope"`
	Kind               string                                `json:"kind"`
	Actor              string                                `json:"actor"`
	ExpectedVersion    string                                `json:"expected_version"`
	PriorReceiptID     string                                `json:"prior_receipt_id"`
	PriorReceiptDigest string                                `json:"prior_receipt_digest"`
	Patch              issueops.RevisionTransitionIssuePatch `json:"patch"`
	ProtectedPermit    string                                `json:"protected_permit"`
}

func (result *TransitionPatchResult) UnmarshalJSON(data []byte) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return errors.New("transition patch result must be a JSON object")
	}
	for name := range object {
		if name != "applied" && name != "replayed" && name != "receipt" {
			return fmt.Errorf("transition patch result has unknown member %q", name)
		}
	}
	if err := decodeRequiredBool(object, "applied", &result.Applied); err != nil {
		return err
	}
	if err := decodeRequiredBool(object, "replayed", &result.Replayed); err != nil {
		return err
	}
	if raw, ok := object["receipt"]; ok && string(raw) != "null" {
		var receipt TransitionPatchReceipt
		if err := json.Unmarshal(raw, &receipt); err != nil {
			return err
		}
		result.Receipt = &receipt
	}
	return nil
}

func decodeRequiredBool(object map[string]json.RawMessage, name string, target *bool) error {
	raw, ok := object[name]
	if !ok {
		return fmt.Errorf("transition patch result is missing %q", name)
	}
	var value *bool
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return fmt.Errorf("transition patch result %q must be boolean", name)
	}
	*target = *value
	return nil
}

func (receipt *TransitionPatchReceipt) UnmarshalJSON(data []byte) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return errors.New("transition patch receipt must be a JSON object")
	}
	allowed := map[string]bool{
		"receipt_id": true, "issue_id": true, "scope": true, "kind": true, "actor": true,
		"expected_version": true, "to_version": true, "prior_receipt_id": true,
		"prior_receipt_digest": true, "patch": true,
	}
	for name := range object {
		if !allowed[name] {
			return fmt.Errorf("transition patch receipt has unknown member %q", name)
		}
	}
	read := func(name string) (string, error) {
		raw, ok := object[name]
		if !ok {
			return "", fmt.Errorf("transition patch receipt is missing %q", name)
		}
		var value *string
		if err := json.Unmarshal(raw, &value); err != nil || value == nil {
			return "", fmt.Errorf("transition patch receipt %q must be a string", name)
		}
		return *value, nil
	}
	var err error
	if receipt.ReceiptID, err = read("receipt_id"); err != nil {
		return err
	}
	if receipt.IssueID, err = read("issue_id"); err != nil {
		return err
	}
	if receipt.Scope, err = read("scope"); err != nil {
		return err
	}
	if receipt.Kind, err = read("kind"); err != nil {
		return err
	}
	if receipt.Actor, err = read("actor"); err != nil {
		return err
	}
	if receipt.PriorReceiptID, err = read("prior_receipt_id"); err != nil {
		return err
	}
	if receipt.PriorReceiptDigest, err = read("prior_receipt_digest"); err != nil {
		return err
	}
	expected, err := read("expected_version")
	if err != nil {
		return err
	}
	if receipt.ExpectedVersion, err = parseRevision(expected); err != nil {
		return fmt.Errorf("expected_version: %w", err)
	}
	to, err := read("to_version")
	if err != nil {
		return err
	}
	if receipt.ToVersion, err = parseRevision(to); err != nil {
		return fmt.Errorf("to_version: %w", err)
	}
	patchRaw, ok := object["patch"]
	if !ok {
		return errors.New("transition patch receipt is missing \"patch\"")
	}
	if receipt.Patch, err = decodeTransitionIssuePatch(patchRaw); err != nil {
		return err
	}
	return nil
}

func decodeTransitionIssuePatch(raw json.RawMessage) (issueops.RevisionTransitionIssuePatch, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return issueops.RevisionTransitionIssuePatch{}, errors.New("patch must be an object")
	}
	allowed := map[string]bool{"metadata": true, "status": true, "assignee": true, "labels": true}
	for name := range object {
		if !allowed[name] {
			return issueops.RevisionTransitionIssuePatch{}, fmt.Errorf("patch has unknown member %q", name)
		}
	}
	var patch issueops.RevisionTransitionIssuePatch
	if metadataRaw, ok := object["metadata"]; ok {
		var entries *[]json.RawMessage
		if err := json.Unmarshal(metadataRaw, &entries); err != nil || entries == nil {
			return patch, errors.New("metadata must be an array")
		}
		for _, rawEntry := range *entries {
			var entry map[string]json.RawMessage
			if err := json.Unmarshal(rawEntry, &entry); err != nil || entry == nil {
				return patch, errors.New("metadata entry must be an object")
			}
			for name := range entry {
				if name != "key" && name != "expected" && name != "value" {
					return patch, fmt.Errorf("metadata entry has unknown member %q", name)
				}
			}
			keyRaw, ok := entry["key"]
			if !ok {
				return patch, errors.New("metadata entry is missing key")
			}
			var key *string
			if err := json.Unmarshal(keyRaw, &key); err != nil || key == nil {
				return patch, errors.New("metadata entry key must be a string")
			}
			patch.Metadata = append(patch.Metadata, issueops.RevisionTransitionMetadataPatch{
				Key: *key, Expected: rawMessagePointer(entry, "expected"), Value: rawMessagePointer(entry, "value"),
			})
		}
	}
	for name, target := range map[string]**issueops.RevisionTransitionStringPatch{"status": &patch.Status, "assignee": &patch.Assignee} {
		if member, ok := object[name]; ok {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(member, &fields); err != nil || fields == nil {
				return patch, fmt.Errorf("%s must be an object", name)
			}
			for key := range fields {
				if key != "expected" && key != "value" {
					return patch, fmt.Errorf("%s has unknown member %q", name, key)
				}
			}
			expected, eok := patchString(fields, "expected")
			value, vok := patchString(fields, "value")
			if !eok || !vok {
				return patch, fmt.Errorf("%s requires expected and value strings", name)
			}
			*target = &issueops.RevisionTransitionStringPatch{Expected: expected, Value: value}
		}
	}
	if member, ok := object["labels"]; ok {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(member, &fields); err != nil || fields == nil {
			return patch, errors.New("labels must be an object")
		}
		for key := range fields {
			if key != "expected" && key != "value" {
				return patch, fmt.Errorf("labels has unknown member %q", key)
			}
		}
		expected, eok := patchStringSlice(fields, "expected")
		value, vok := patchStringSlice(fields, "value")
		if !eok || !vok {
			return patch, errors.New("labels requires expected and value arrays")
		}
		patch.Labels = &issueops.RevisionTransitionLabelsPatch{Expected: expected, Value: value}
	}
	return patch, nil
}

func rawMessagePointer(object map[string]json.RawMessage, name string) *json.RawMessage {
	raw, ok := object[name]
	if !ok {
		return nil
	}
	copy := append(json.RawMessage(nil), raw...)
	return &copy
}

func patchString(object map[string]json.RawMessage, name string) (string, bool) {
	raw, ok := object[name]
	if !ok {
		return "", false
	}
	var value *string
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return "", false
	}
	return *value, utf8.ValidString(*value)
}

func patchStringSlice(object map[string]json.RawMessage, name string) ([]string, bool) {
	raw, ok := object[name]
	if !ok {
		return nil, false
	}
	var values *[]string
	if err := json.Unmarshal(raw, &values); err != nil || values == nil {
		return nil, false
	}
	for _, value := range *values {
		if !utf8.ValidString(value) {
			return nil, false
		}
	}
	return *values, true
}
