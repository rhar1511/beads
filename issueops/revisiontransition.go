package issueops

import (
	"context"
	"encoding/json"
)

// RevisionTransitionRequest describes one idempotent metadata transition that
// must be fenced by the issue's exact opaque row revision. ReceiptID is
// caller-generated and must remain stable across retries of the same request.
type RevisionTransitionRequest struct {
	ProtectedPermit ProtectedMutationPermit
	ReceiptID       string
	IssueID         string
	Scope           string
	Kind            string
	Actor           string
	ExpectedVersion int64
	Key             string
	Expected        *json.RawMessage
	Value           *json.RawMessage
	Payload         json.RawMessage
}

// RevisionTransitionPatchRequest describes one atomic, revision-fenced source
// update. PriorReceiptID and PriorReceiptDigest bind this step to the exact
// durable receipt that produced ExpectedVersion. The protected permit is
// excluded from the canonical request identity and is verified by storage.
type RevisionTransitionPatchRequest struct {
	ProtectedPermit    ProtectedMutationPermit      `json:"-"`
	ReceiptID          string                       `json:"receipt_id"`
	IssueID            string                       `json:"issue_id"`
	Scope              string                       `json:"scope"`
	Kind               string                       `json:"kind"`
	Actor              string                       `json:"actor"`
	ExpectedVersion    int64                        `json:"expected_version"`
	PriorReceiptID     string                       `json:"prior_receipt_id"`
	PriorReceiptDigest string                       `json:"prior_receipt_digest"`
	Patch              RevisionTransitionIssuePatch `json:"patch"`
}

// RevisionTransitionIssuePatch is a canonical compare-and-set patch. Every
// supplied field carries both its expected and next values. Metadata entries
// are sorted by key; labels are sorted exact sets.
type RevisionTransitionIssuePatch struct {
	Metadata []RevisionTransitionMetadataPatch `json:"metadata,omitempty"`
	Status   *RevisionTransitionStringPatch    `json:"status,omitempty"`
	Assignee *RevisionTransitionStringPatch    `json:"assignee,omitempty"`
	Labels   *RevisionTransitionLabelsPatch    `json:"labels,omitempty"`
}

type RevisionTransitionMetadataPatch struct {
	Key      string           `json:"key"`
	Expected *json.RawMessage `json:"expected,omitempty"`
	Value    *json.RawMessage `json:"value,omitempty"`
}

type RevisionTransitionStringPatch struct {
	Expected string `json:"expected"`
	Value    string `json:"value"`
}

type RevisionTransitionLabelsPatch struct {
	Expected []string `json:"expected"`
	Value    []string `json:"value"`
}

// RevisionTransitionPatchReceipt is an immutable record of one source patch.
// ToVersion is the backend-generated revision committed with the patch and
// receipt in the same transaction.
type RevisionTransitionPatchReceipt struct {
	ReceiptID          string                       `json:"receipt_id"`
	IssueID            string                       `json:"issue_id"`
	Scope              string                       `json:"scope"`
	Kind               string                       `json:"kind"`
	Actor              string                       `json:"actor"`
	ExpectedVersion    int64                        `json:"expected_version"`
	ToVersion          int64                        `json:"to_version"`
	PriorReceiptID     string                       `json:"prior_receipt_id"`
	PriorReceiptDigest string                       `json:"prior_receipt_digest"`
	Patch              RevisionTransitionIssuePatch `json:"patch"`
}

// RevisionTransitionPatchResult distinguishes a newly applied operation from
// an exact replay. A replay returns the original receipt; it does not claim the
// source row still has that receipt's ToVersion.
type RevisionTransitionPatchResult struct {
	Applied  bool
	Replayed bool
	Receipt  *RevisionTransitionPatchReceipt
}

// RevisionTransitionReceipt is the immutable record of an applied transition.
// ToVersion is minted by the storage backend and is the exact row_lock value
// written to the issue in the same transaction as the marker and receipt.
type RevisionTransitionReceipt struct {
	ReceiptID       string
	IssueID         string
	Scope           string
	Kind            string
	Actor           string
	ExpectedVersion int64
	ToVersion       int64
	Key             string
	Expected        *json.RawMessage
	Value           *json.RawMessage
	Payload         json.RawMessage
}

// RevisionTransitionResult reports either a marker refusal or an applied
// transition. A replay returns the original receipt with Replayed set; it does
// not claim that the marker still has that value now.
type RevisionTransitionResult struct {
	Applied  bool
	Replayed bool
	Current  *json.RawMessage
	Receipt  *RevisionTransitionReceipt
}

// RevisionTransition atomically checks one issue revision and one metadata
// marker, changes the marker, mints the resulting row revision, and persists an
// immutable receipt containing that exact result. It does not authorize the
// caller; authorization and higher-level lifecycle fences belong to the
// controller boundary. This role does not provide a claim fence.
type RevisionTransition interface {
	TransitionMetadata(context.Context, RevisionTransitionRequest) (RevisionTransitionResult, error)
	GetReceipt(context.Context, string) (*RevisionTransitionReceipt, error)
}

// RevisionTransitionPatcher is the additive typed patch capability. It is
// kept separate from RevisionTransition so existing role implementations and
// the compatible one-key metadata API do not need to change.
type RevisionTransitionPatcher interface {
	TransitionPatch(context.Context, RevisionTransitionPatchRequest) (RevisionTransitionPatchResult, error)
	GetPatchReceipt(context.Context, string) (*RevisionTransitionPatchReceipt, error)
}
