package httpapi

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/issueops"
)

var transitionPatchRequestMembers = []string{
	"receipt_id", "scope", "kind", "actor", "expected_version",
	"prior_receipt_id", "prior_receipt_digest", "patch", "protected_permit",
}

// handleTransitionPatch is available only after the additive receipt schema,
// a typed patch role, bearer/project identity, and protected-mutation
// authority have all been proven at startup.
func (s *Server) handleTransitionPatch(w http.ResponseWriter, r *http.Request) {
	if !s.revisionPatchReady {
		s.fail(w, r, TransitionNotImplemented())
		return
	}
	id := r.PathValue(customMethodIDValue)
	if !s.requireNoQuery(w, r) || !s.requireJSONContent(w, r) {
		return
	}
	request, ok := s.revisionTransitionPatchRequest(w, r, id)
	if !ok {
		return
	}
	role, err := s.revisionTransition(r)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	patcher, ok := role.(issueops.RevisionTransitionPatcher)
	if !ok || isNilRevisionTransitionPatcher(patcher) {
		s.fail(w, r, TransitionNotImplemented())
		return
	}
	result, err := patcher.TransitionPatch(r.Context(), request)
	if err != nil {
		s.failTransitionPatch(w, r, request, err)
		return
	}
	if result.Replayed && (!result.Applied || result.Receipt == nil) ||
		result.Applied && result.Receipt == nil || !result.Applied && result.Receipt != nil ||
		result.Receipt != nil && !transitionPatchReceiptMatchesRequest(request, result.Receipt) {
		s.failErr(w, r, errors.New("revision transition patch role returned an inconsistent result"))
		return
	}
	var receipt any
	if result.Receipt != nil {
		receipt = transitionPatchReceiptWire(result.Receipt)
	}
	writeJSON(w, struct {
		Applied  bool `json:"applied"`
		Replayed bool `json:"replayed"`
		Receipt  any  `json:"receipt,omitempty"`
	}{Applied: result.Applied, Replayed: result.Replayed, Receipt: receipt})
}

func (s *Server) revisionTransitionPatchRequest(w http.ResponseWriter, r *http.Request, id string) (issueops.RevisionTransitionPatchRequest, bool) {
	bad := func(param, detail string) (issueops.RevisionTransitionPatchRequest, bool) {
		s.fail(w, r, InvalidArgument(param, ReasonInvalidValue, detail))
		return issueops.RevisionTransitionPatchRequest{}, false
	}
	members, res := decodeJSONObjectBody(w, r)
	if res != nil {
		s.fail(w, r, *res)
		return issueops.RevisionTransitionPatchRequest{}, false
	}
	if offender, unknown := unknownMember(members, transitionPatchRequestMembers); unknown {
		s.failUnknownMember(w, r, offender, transitionPatchRequestMembers)
		return issueops.RevisionTransitionPatchRequest{}, false
	}
	receiptID, ok := requiredStringMember(members, "receipt_id")
	if !ok {
		return bad("receipt_id", "`receipt_id` is required and must be a string")
	}
	if err := storage.ValidateRevisionTransitionReceiptID(receiptID); err != nil {
		return bad("receipt_id", "`receipt_id` must be valid nonempty text of at most 191 bytes")
	}
	scope, ok := requiredBoundedStringMember(members, "scope", 255)
	if !ok {
		return bad("scope", "`scope` is required and must be a nonempty string of at most 255 characters")
	}
	kind, ok := requiredBoundedStringMember(members, "kind", 64)
	if !ok {
		return bad("kind", "`kind` is required and must be a nonempty string of at most 64 characters")
	}
	actor, ok := s.bodyActor(w, r, members)
	if !ok {
		return issueops.RevisionTransitionPatchRequest{}, false
	}
	expectedVersionToken, ok := requiredStringMember(members, "expected_version")
	expectedVersion, err := strconv.ParseInt(expectedVersionToken, 10, 64)
	if !ok || err != nil || expectedVersion == 0 || types.RevisionToken(expectedVersion) != expectedVersionToken {
		return bad("expected_version", "`expected_version` must be a nonzero signed 64-bit decimal string")
	}
	priorReceiptID, ok := requiredStringMember(members, "prior_receipt_id")
	if !ok {
		return bad("prior_receipt_id", "`prior_receipt_id` is required and must be a string")
	}
	priorDigest, ok := requiredStringMember(members, "prior_receipt_digest")
	if !ok || len(priorDigest) != 64 || strings.ToLower(priorDigest) != priorDigest {
		return bad("prior_receipt_digest", "`prior_receipt_digest` must be 64 lowercase hexadecimal characters")
	}
	if _, err := hex.DecodeString(priorDigest); err != nil {
		return bad("prior_receipt_digest", "`prior_receipt_digest` must be 64 lowercase hexadecimal characters")
	}
	patchRaw, ok := members["patch"]
	if !ok {
		return bad("patch", "`patch` is required and must be an object")
	}
	patch, err := parseTransitionPatch(patchRaw)
	if err != nil {
		return bad("patch", "`patch` must be a valid compare-and-set issue patch")
	}
	permitRaw, ok := members["protected_permit"]
	if !ok {
		return bad("protected_permit", "`protected_permit` is required for this operation")
	}
	permit, permitRes := protectedMutationPermitMember(map[string]json.RawMessage{"protected_permit": permitRaw})
	if permitRes != nil {
		s.fail(w, r, *permitRes)
		return issueops.RevisionTransitionPatchRequest{}, false
	}
	request := issueops.RevisionTransitionPatchRequest{
		ProtectedPermit: permit, ReceiptID: receiptID, IssueID: id,
		Scope: scope, Kind: kind, Actor: actor, ExpectedVersion: expectedVersion,
		PriorReceiptID: priorReceiptID, PriorReceiptDigest: priorDigest, Patch: patch,
	}
	plan, err := storage.PlanRevisionTransitionPatch(request)
	if err != nil {
		return bad("patch", "the revision transition patch request is invalid")
	}
	request.ReceiptID, request.IssueID, request.Scope = plan.ReceiptID, plan.IssueID, plan.Scope
	request.Kind, request.Actor, request.ExpectedVersion = plan.Kind, plan.Actor, plan.ExpectedVersion
	request.PriorReceiptID, request.PriorReceiptDigest, request.Patch = plan.PriorReceiptID, plan.PriorReceiptDigest, plan.Patch
	return request, true
}

func parseTransitionPatch(raw json.RawMessage) (issueops.RevisionTransitionIssuePatch, error) {
	patchMembers, err := rawJSONObject(raw)
	if err != nil {
		return issueops.RevisionTransitionIssuePatch{}, err
	}
	if offender, unknown := unknownMember(patchMembers, []string{"metadata", "status", "assignee", "labels"}); unknown {
		return issueops.RevisionTransitionIssuePatch{}, fmt.Errorf("unknown patch member %q", offender)
	}
	var patch issueops.RevisionTransitionIssuePatch
	if metadataRaw, present := patchMembers["metadata"]; present {
		var entries *[]json.RawMessage
		if err := json.Unmarshal(metadataRaw, &entries); err != nil || entries == nil {
			return issueops.RevisionTransitionIssuePatch{}, errors.New("metadata must be an array")
		}
		for _, rawEntry := range *entries {
			entry, err := rawJSONObject(rawEntry)
			if err != nil {
				return issueops.RevisionTransitionIssuePatch{}, err
			}
			if offender, unknown := unknownMember(entry, []string{"key", "expected", "value"}); unknown {
				return issueops.RevisionTransitionIssuePatch{}, fmt.Errorf("unknown metadata member %q", offender)
			}
			key, ok := requiredStringMember(entry, "key")
			if !ok {
				return issueops.RevisionTransitionIssuePatch{}, errors.New("metadata key is required")
			}
			patch.Metadata = append(patch.Metadata, issueops.RevisionTransitionMetadataPatch{
				Key: key, Expected: metadataCASMember(entry, "expected"), Value: metadataCASMember(entry, "value"),
			})
		}
	}
	if value, present := patchMembers["status"]; present {
		patch.Status, err = parseTransitionStringPatch(value)
		if err != nil {
			return issueops.RevisionTransitionIssuePatch{}, err
		}
	}
	if value, present := patchMembers["assignee"]; present {
		patch.Assignee, err = parseTransitionStringPatch(value)
		if err != nil {
			return issueops.RevisionTransitionIssuePatch{}, err
		}
	}
	if value, present := patchMembers["labels"]; present {
		patch.Labels, err = parseTransitionLabelsPatch(value)
		if err != nil {
			return issueops.RevisionTransitionIssuePatch{}, err
		}
	}
	return patch, nil
}

func parseTransitionStringPatch(raw json.RawMessage) (*issueops.RevisionTransitionStringPatch, error) {
	members, err := rawJSONObject(raw)
	if err != nil {
		return nil, err
	}
	if offender, unknown := unknownMember(members, []string{"expected", "value"}); unknown {
		return nil, fmt.Errorf("unknown string patch member %q", offender)
	}
	expected, ok := requiredStringMember(members, "expected")
	if !ok {
		return nil, errors.New("expected is required")
	}
	value, ok := requiredStringMember(members, "value")
	if !ok {
		return nil, errors.New("value is required")
	}
	return &issueops.RevisionTransitionStringPatch{Expected: expected, Value: value}, nil
}

func parseTransitionLabelsPatch(raw json.RawMessage) (*issueops.RevisionTransitionLabelsPatch, error) {
	members, err := rawJSONObject(raw)
	if err != nil {
		return nil, err
	}
	if offender, unknown := unknownMember(members, []string{"expected", "value"}); unknown {
		return nil, fmt.Errorf("unknown labels patch member %q", offender)
	}
	expected, ok := requiredStringSliceMember(members, "expected")
	if !ok {
		return nil, errors.New("expected labels are required")
	}
	value, ok := requiredStringSliceMember(members, "value")
	if !ok {
		return nil, errors.New("next labels are required")
	}
	return &issueops.RevisionTransitionLabelsPatch{Expected: expected, Value: value}, nil
}

func requiredStringSliceMember(members map[string]json.RawMessage, name string) ([]string, bool) {
	raw, ok := members[name]
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

func rawJSONObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil || members == nil {
		return nil, errors.New("value must be an object")
	}
	return members, nil
}

func (s *Server) failTransitionPatch(w http.ResponseWriter, r *http.Request, request issueops.RevisionTransitionPatchRequest, err error) {
	switch {
	case errors.Is(err, storage.ErrNotFound):
		s.fail(w, r, NotFound())
	case errors.Is(err, storage.ErrVersionMismatch):
		s.fail(w, r, PreconditionFailed().WithExpectedVersion(request.ExpectedVersion))
	case errors.Is(err, issueops.ErrRevisionPatchPreconditionFailed):
		s.fail(w, r, PreconditionFailed())
	case errors.Is(err, issueops.ErrTransitionReceiptConflict):
		s.fail(w, r, TransitionReceiptConflict())
	case errors.Is(err, issueops.ErrProtectedRecord):
		s.fail(w, r, ProtectedRecordRefused())
	default:
		var unsupported *storage.ErrUnsupported
		if errors.As(err, &unsupported) {
			s.fail(w, r, UnsupportedTransitionResource())
			return
		}
		if errors.Is(err, storage.ErrValidation) {
			s.fail(w, r, InvalidArgument("patch", ReasonInvalidValue, "the revision transition patch request is invalid"))
			return
		}
		s.failErr(w, r, err)
	}
}

func (s *Server) handleGetTransitionPatchReceipt(w http.ResponseWriter, r *http.Request) {
	if !s.revisionPatchReady {
		s.fail(w, r, TransitionNotImplemented())
		return
	}
	if !s.requireNoQuery(w, r) {
		return
	}
	receiptID := r.PathValue("receipt_id")
	if err := storage.ValidateRevisionTransitionReceiptID(receiptID); err != nil {
		s.fail(w, r, InvalidArgument("receipt_id", ReasonInvalidValue,
			"`receipt_id` must be valid nonempty text of at most 191 bytes"))
		return
	}
	role, err := s.revisionTransition(r)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	patcher, ok := role.(issueops.RevisionTransitionPatcher)
	if !ok || isNilRevisionTransitionPatcher(patcher) {
		s.fail(w, r, TransitionNotImplemented())
		return
	}
	receipt, err := patcher.GetPatchReceipt(r.Context(), receiptID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			s.fail(w, r, TransitionReceiptNotFound())
			return
		}
		if errors.Is(err, storage.ErrValidation) {
			s.fail(w, r, InvalidArgument("receipt_id", ReasonInvalidValue, "`receipt_id` is invalid"))
			return
		}
		s.failErr(w, r, err)
		return
	}
	if receipt == nil || receipt.ReceiptID != receiptID || validateTransitionPatchReceipt(receipt) != nil {
		s.failErr(w, r, errors.New("revision transition patch role returned an invalid receipt"))
		return
	}
	writeJSON(w, transitionPatchReceiptWire(receipt))
}

func transitionPatchReceiptWire(receipt *issueops.RevisionTransitionPatchReceipt) any {
	return struct {
		ReceiptID          string                                `json:"receipt_id"`
		IssueID            string                                `json:"issue_id"`
		Scope              string                                `json:"scope"`
		Kind               string                                `json:"kind"`
		Actor              string                                `json:"actor"`
		ExpectedVersion    string                                `json:"expected_version"`
		ToVersion          string                                `json:"to_version"`
		PriorReceiptID     string                                `json:"prior_receipt_id"`
		PriorReceiptDigest string                                `json:"prior_receipt_digest"`
		Patch              issueops.RevisionTransitionIssuePatch `json:"patch"`
	}{
		ReceiptID: receipt.ReceiptID, IssueID: receipt.IssueID, Scope: receipt.Scope,
		Kind: receipt.Kind, Actor: receipt.Actor,
		ExpectedVersion: types.RevisionToken(receipt.ExpectedVersion), ToVersion: types.RevisionToken(receipt.ToVersion),
		PriorReceiptID: receipt.PriorReceiptID, PriorReceiptDigest: receipt.PriorReceiptDigest, Patch: receipt.Patch,
	}
}

func transitionPatchReceiptMatchesRequest(request issueops.RevisionTransitionPatchRequest, receipt *issueops.RevisionTransitionPatchReceipt) bool {
	plan, err := storage.PlanRevisionTransitionPatch(request)
	return err == nil && validateTransitionPatchReceipt(receipt) == nil && storage.SameRevisionTransitionPatchRequest(plan, receipt)
}

func validateTransitionPatchReceipt(receipt *issueops.RevisionTransitionPatchReceipt) error {
	if receipt == nil || receipt.ToVersion == 0 || receipt.ToVersion == receipt.ExpectedVersion {
		return errors.New("patch receipt has invalid revisions")
	}
	request := issueops.RevisionTransitionPatchRequest{
		ReceiptID: receipt.ReceiptID, IssueID: receipt.IssueID, Scope: receipt.Scope,
		Kind: receipt.Kind, Actor: receipt.Actor, ExpectedVersion: receipt.ExpectedVersion,
		PriorReceiptID: receipt.PriorReceiptID, PriorReceiptDigest: receipt.PriorReceiptDigest,
		Patch: receipt.Patch,
	}
	plan, err := storage.PlanRevisionTransitionPatch(request)
	if err != nil {
		return err
	}
	if !storage.SameRevisionTransitionPatchRequest(plan, receipt) {
		return errors.New("patch receipt is not canonically bound to its request")
	}
	return nil
}
