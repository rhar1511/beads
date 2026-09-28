package issueops

import (
	"context"
	"encoding/json"
)

// RevisionTransitionRequest describes one idempotent metadata transition that
// must be fenced by the issue's exact opaque row revision. ReceiptID is
// caller-generated and must remain stable across retries of the same request.
type RevisionTransitionRequest struct {
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
