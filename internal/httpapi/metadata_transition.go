package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/uow"
)

var metadataTransitionRequestMembers = []string{
	"receipt_id", "scope", "kind", "actor", "expected_version", "key", "expected", "value", "payload", "protected_permit",
}

func (s *Server) handleTransitionMetadata(w http.ResponseWriter, r *http.Request) {
	if !s.requireNoQuery(w, r) || !s.requireJSONContent(w, r) {
		return
	}
	if s.provider == nil {
		s.fail(w, r, newResult(CodeDBUnavailable, "metadata transitions require a transactional provider"))
		return
	}
	members, res := decodeJSONObjectBody(w, r)
	if res != nil {
		s.fail(w, r, *res)
		return
	}
	if offender, unknown := unknownMember(members, metadataTransitionRequestMembers); unknown {
		s.failUnknownMember(w, r, offender, metadataTransitionRequestMembers)
		return
	}
	member := func(name string) string {
		var value string
		_ = json.Unmarshal(members[name], &value)
		return value
	}
	req := uow.MetadataTransitionRequest{
		IssueID: r.PathValue(customMethodIDValue), ReceiptID: member("receipt_id"),
		Scope: member("scope"), Kind: member("kind"), Actor: member("actor"),
		ExpectedVersion: member("expected_version"), Key: member("key"),
		Expected: metadataCASMember(members, "expected"), Value: metadataCASMember(members, "value"),
		ProtectedPermit: member("protected_permit"), Payload: members["payload"],
	}
	for _, name := range []string{"receipt_id", "scope", "kind", "actor", "expected_version", "key"} {
		if reqField := member(name); reqField == "" || strings.ContainsFunc(reqField, isControlChar) {
			s.fail(w, r, InvalidArgument(name, ReasonInvalidValue, "a nonempty string is required"))
			return
		}
	}
	if _, supplied := members["protected_permit"]; supplied {
		s.fail(w, r, InvalidArgument("protected_permit", ReasonInvalidValue, "protected transitions are unavailable"))
		return
	}
	result, err := uow.TransitionMetadata(r.Context(), timedProvider{inner: s.provider, rec: requestInfo(r.Context())}, req)
	if err != nil {
		switch {
		case errors.Is(err, storage.ErrNotFound):
			s.fail(w, r, NotFound())
		case errors.Is(err, uow.ErrTransitionPrecondition), errors.Is(err, storage.ErrVersionMismatch), errors.Is(err, uow.ErrTransitionReceiptConflict):
			s.fail(w, r, PreconditionFailed())
		case errors.Is(err, storage.ErrValidation):
			s.fail(w, r, InvalidArgument("key", ReasonInvalidValue, "metadata transition was refused"))
		default:
			s.failErr(w, r, err)
		}
		return
	}
	writeJSON(w, result)
}

func (s *Server) handleGetTransitionReceipt(w http.ResponseWriter, r *http.Request) {
	if !s.requireNoQuery(w, r) {
		return
	}
	if s.provider == nil {
		s.fail(w, r, newResult(CodeDBUnavailable, "transition receipts require a transactional provider"))
		return
	}
	id := r.PathValue("receiptID")
	if id == "" || len(id) > 255 || strings.ContainsFunc(id, isControlChar) {
		s.fail(w, r, NotFound())
		return
	}
	receipt, found, err := uow.MetadataTransitionReceiptByID(r.Context(),
		timedProvider{inner: s.provider, rec: requestInfo(r.Context())}, id)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	if !found {
		s.fail(w, r, NotFound())
		return
	}
	writeJSON(w, receipt)
}
