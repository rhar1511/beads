package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/steveyegge/beads/internal/httpapi/apigen"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/issueops"
)

var transitionMetadataRequestMembers = []string{
	"receipt_id", "scope", "kind", "actor", "expected_version", "key", "expected", "value", "payload", "protected_permit",
}

// handleTransitionMetadata applies one revision-fenced metadata transition.
// A marker miss is a successful 200 carrying the marker observed by storage;
// only a stale row revision or a receipt-ID binding collision is a conflict.
func (s *Server) handleTransitionMetadata(w http.ResponseWriter, r *http.Request) {
	if !s.revisionTransitionReady {
		s.fail(w, r, TransitionNotImplemented())
		return
	}
	id := r.PathValue(customMethodIDValue)
	if !s.requireNoQuery(w, r) || !s.requireJSONContent(w, r) {
		return
	}
	request, ok := s.revisionTransitionRequest(w, r, id)
	if !ok {
		return
	}
	role, err := s.revisionTransition(r)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	if role == nil {
		s.fail(w, r, TransitionNotImplemented())
		return
	}
	result, err := role.TransitionMetadata(r.Context(), request)
	if err != nil {
		s.failTransitionMetadata(w, r, request, err)
		return
	}
	if result.Replayed && (!result.Applied || result.Receipt == nil) || result.Applied && result.Receipt == nil || !result.Applied && result.Receipt != nil || result.Receipt != nil && !transitionReceiptMatchesRequest(request, result.Receipt) {
		s.failErr(w, r, fmt.Errorf("revision transition role returned an inconsistent result"))
		return
	}
	response := apigen.TransitionMetadataResponse{
		Applied:  result.Applied,
		Replayed: result.Replayed,
		Current:  metadataCASWireValue(result.Current),
	}
	if result.Receipt != nil {
		response.Receipt = transitionReceiptWire(result.Receipt)
	}
	writeJSON(w, response)
}

func (s *Server) revisionTransitionRequest(w http.ResponseWriter, r *http.Request, id string) (issueops.RevisionTransitionRequest, bool) {
	bad := func(param, detail string) (issueops.RevisionTransitionRequest, bool) {
		s.fail(w, r, InvalidArgument(param, ReasonInvalidValue, detail))
		return issueops.RevisionTransitionRequest{}, false
	}
	members, res := decodeJSONObjectBody(w, r)
	if res != nil {
		s.fail(w, r, *res)
		return issueops.RevisionTransitionRequest{}, false
	}
	if offender, unknown := unknownMember(members, transitionMetadataRequestMembers); unknown {
		s.failUnknownMember(w, r, offender, transitionMetadataRequestMembers)
		return issueops.RevisionTransitionRequest{}, false
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
		return issueops.RevisionTransitionRequest{}, false
	}
	expectedVersion, ok := requiredRevisionMember(members, "expected_version")
	if !ok || expectedVersion == 0 {
		return bad("expected_version", "`expected_version` must be a nonzero signed 64-bit decimal string")
	}
	key, keyRes := requiredEndpointMember(members, "key")
	if keyRes != nil {
		s.fail(w, r, *keyRes)
		return issueops.RevisionTransitionRequest{}, false
	}
	if err := storage.ValidateMetadataKey(key); err != nil {
		return bad("key", "`key` is not a valid metadata key")
	}
	if types.CheckFieldLen("key", key) != nil {
		return bad("key", "`key` exceeds 255 characters")
	}

	expected := metadataCASMember(members, "expected")
	value := metadataCASMember(members, "value")
	if _, err := storage.CanonicalMetadataPointer(expected); err != nil {
		return bad("expected", "`expected` must be a JSON value")
	}
	if _, err := storage.CanonicalMetadataPointer(value); err != nil {
		return bad("value", "`value` must be a JSON value")
	}
	unchanged, err := storage.MetadataValuesEqual(expected, value)
	if err != nil {
		return bad("value", "`value` must be a JSON value")
	}
	if unchanged {
		return bad("value", "the transition must change the metadata marker")
	}
	payload := metadataCASMember(members, "payload")
	if payload != nil {
		if _, err := storage.CanonicalMetadataValue(*payload); err != nil {
			return bad("payload", "`payload` must be a JSON value")
		}
	}
	var protectedPermit issueops.ProtectedMutationPermit
	if _, present := members["protected_permit"]; present {
		token, ok := requiredStringMember(members, "protected_permit")
		if !ok || token == "" || len(token) > maxProtectedMutationPermitBytes ||
			!utf8.ValidString(token) || strings.TrimSpace(token) != token || hasProtectedMutationControl(token) {
			return bad("protected_permit", "`protected_permit` must be a nonempty opaque token of at most 65536 bytes")
		}
		protectedPermit = issueops.ProtectedMutationPermit(token)
	}

	request := issueops.RevisionTransitionRequest{
		ProtectedPermit: protectedPermit,
		ReceiptID:       receiptID,
		IssueID:         id,
		Scope:           scope,
		Kind:            kind,
		Actor:           actor,
		ExpectedVersion: expectedVersion,
		Key:             key,
		Expected:        expected,
		Value:           value,
	}
	if payload != nil {
		request.Payload = *payload
	}
	plan, err := storage.PlanRevisionTransition(request)
	if err != nil {
		return bad("", "the revision transition request is invalid")
	}
	return issueops.RevisionTransitionRequest{
		ProtectedPermit: request.ProtectedPermit,
		ReceiptID:       plan.ReceiptID,
		IssueID:         plan.IssueID,
		Scope:           plan.Scope,
		Kind:            plan.Kind,
		Actor:           plan.Actor,
		ExpectedVersion: plan.ExpectedVersion,
		Key:             plan.Key,
		Expected:        plan.Expected,
		Value:           plan.Value,
		Payload:         plan.Payload,
	}, true
}

func hasProtectedMutationControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return true
		}
	}
	return false
}

func requiredStringMember(members map[string]json.RawMessage, name string) (string, bool) {
	raw, ok := members[name]
	if !ok {
		return "", false
	}
	var value *string
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return "", false
	}
	return *value, true
}

func requiredBoundedStringMember(members map[string]json.RawMessage, name string, maxRunes int) (string, bool) {
	value, ok := requiredStringMember(members, name)
	if !ok || value == "" || utf8.RuneCountInString(value) > maxRunes {
		return "", false
	}
	return value, true
}

func requiredRevisionMember(members map[string]json.RawMessage, name string) (int64, bool) {
	token, ok := requiredStringMember(members, name)
	if !ok {
		return 0, false
	}
	value, err := strconv.ParseInt(token, 10, 64)
	if err != nil || types.RevisionToken(value) != token {
		return 0, false
	}
	return value, true
}

func (s *Server) failTransitionMetadata(w http.ResponseWriter, r *http.Request, request issueops.RevisionTransitionRequest, err error) {
	switch {
	case errors.Is(err, storage.ErrNotFound):
		s.fail(w, r, NotFound())
	case errors.Is(err, issueops.ErrVersionMismatch):
		s.fail(w, r, PreconditionFailed().WithExpectedVersion(request.ExpectedVersion))
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
			s.fail(w, r, InvalidArgument("", ReasonInvalidValue, "the revision transition request is invalid"))
			return
		}
		s.failErr(w, r, err)
	}
}

// handleGetTransitionReceipt reads one immutable receipt by its exact ID.
func (s *Server) handleGetTransitionReceipt(w http.ResponseWriter, r *http.Request) {
	if !s.revisionTransitionReady {
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
	if role == nil {
		s.fail(w, r, TransitionNotImplemented())
		return
	}
	receipt, err := role.GetReceipt(r.Context(), receiptID)
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
	if receipt == nil {
		s.failErr(w, r, errors.New("revision transition role returned a nil receipt without an error"))
		return
	}
	if receipt.ReceiptID != receiptID {
		s.failErr(w, r, errors.New("revision transition role returned a receipt with a different ID"))
		return
	}
	if err := validateTransitionReceipt(receipt); err != nil {
		s.failErr(w, r, fmt.Errorf("revision transition role returned an invalid receipt: %w", err))
		return
	}
	writeJSON(w, transitionReceiptWire(receipt))
}

func transitionReceiptWire(receipt *issueops.RevisionTransitionReceipt) *apigen.TransitionReceipt {
	if receipt == nil {
		return nil
	}
	wire := &apigen.TransitionReceipt{
		ReceiptId:       receipt.ReceiptID,
		IssueId:         receipt.IssueID,
		Scope:           receipt.Scope,
		Kind:            receipt.Kind,
		Actor:           receipt.Actor,
		ExpectedVersion: types.RevisionToken(receipt.ExpectedVersion),
		ToVersion:       types.RevisionToken(receipt.ToVersion),
		Key:             receipt.Key,
		Expected:        metadataCASWireValue(receipt.Expected),
		Value:           metadataCASWireValue(receipt.Value),
		Payload:         apigen.MetadataValue(append([]byte(nil), receipt.Payload...)),
	}
	return wire
}

// transitionReceiptMatchesRequest is useful to downstream clients during
// response-loss recovery; this server uses the same storage comparator before
// allowing a receipt replay to escape the storage role.
func transitionReceiptMatchesRequest(request issueops.RevisionTransitionRequest, receipt *issueops.RevisionTransitionReceipt) bool {
	plan, err := storage.PlanRevisionTransition(request)
	return err == nil && validateTransitionReceipt(receipt) == nil && storage.SameRevisionTransitionRequest(plan, receipt)
}

// validateTransitionReceipt verifies the receipt's canonical request binding
// and the revision invariants before any role-provided receipt reaches the
// wire. Storage reads back the resulting row revision; the HTTP boundary also
// refuses values no transition can validly produce.
func validateTransitionReceipt(receipt *issueops.RevisionTransitionReceipt) error {
	if receipt == nil {
		return errors.New("revision transition role returned a nil receipt")
	}
	if receipt.ToVersion == 0 || receipt.ToVersion == receipt.ExpectedVersion {
		return errors.New("revision transition receipt has an invalid resulting revision")
	}
	request := issueops.RevisionTransitionRequest{
		ReceiptID:       receipt.ReceiptID,
		IssueID:         receipt.IssueID,
		Scope:           receipt.Scope,
		Kind:            receipt.Kind,
		Actor:           receipt.Actor,
		ExpectedVersion: receipt.ExpectedVersion,
		Key:             receipt.Key,
		Expected:        receipt.Expected,
		Value:           receipt.Value,
		Payload:         receipt.Payload,
	}
	plan, err := storage.PlanRevisionTransition(request)
	if err != nil {
		return err
	}
	if !storage.SameRevisionTransitionRequest(plan, receipt) {
		return errors.New("revision transition receipt is not canonically bound to its request fields")
	}
	return nil
}
