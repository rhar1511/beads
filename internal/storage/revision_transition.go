package storage

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	publicops "github.com/steveyegge/beads/issueops"
)

// RevisionTransitionPlan is the validated immutable request consumed by the
// transaction body. Optional marker values are canonical JSON; nil means the
// key is absent. A nil caller payload is normalized to an empty object.
type RevisionTransitionPlan struct {
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

// PlanRevisionTransition validates and snapshots every caller-owned field
// before a transaction or connection is opened.
func PlanRevisionTransition(in publicops.RevisionTransitionRequest) (RevisionTransitionPlan, error) {
	if err := ValidateRevisionTransitionReceiptID(in.ReceiptID); err != nil {
		return RevisionTransitionPlan{}, err
	}
	validateText := func(name, value string, max int) error {
		if value == "" {
			return fmt.Errorf("%w: revision transition requires %s", publicops.ErrValidation, name)
		}
		if utf8.RuneCountInString(value) > max {
			return fmt.Errorf("%w: revision transition %s exceeds %d characters", publicops.ErrValidation, name, max)
		}
		return nil
	}
	for _, field := range []struct {
		name  string
		value string
		max   int
	}{
		{"issue id", in.IssueID, 255},
		{"scope", in.Scope, 255},
		{"kind", in.Kind, 64},
		{"actor", in.Actor, 255},
	} {
		if err := validateText(field.name, field.value, field.max); err != nil {
			return RevisionTransitionPlan{}, err
		}
	}
	if in.ExpectedVersion == 0 {
		return RevisionTransitionPlan{}, fmt.Errorf("%w: expected issue revision must be nonzero", publicops.ErrValidation)
	}
	if err := ValidateMetadataKey(in.Key); err != nil {
		return RevisionTransitionPlan{}, fmt.Errorf("%w: %v", publicops.ErrValidation, err)
	}
	if err := validateText("metadata key", in.Key, 255); err != nil {
		return RevisionTransitionPlan{}, err
	}

	plan := RevisionTransitionPlan{
		ReceiptID:       in.ReceiptID,
		IssueID:         in.IssueID,
		Scope:           in.Scope,
		Kind:            in.Kind,
		Actor:           in.Actor,
		ExpectedVersion: in.ExpectedVersion,
		Key:             in.Key,
	}
	var err error
	if plan.Expected, err = CanonicalMetadataPointer(in.Expected); err != nil {
		return RevisionTransitionPlan{}, fmt.Errorf("%w: expected marker: %v", publicops.ErrValidation, err)
	}
	if plan.Value, err = CanonicalMetadataPointer(in.Value); err != nil {
		return RevisionTransitionPlan{}, fmt.Errorf("%w: next marker: %v", publicops.ErrValidation, err)
	}
	unchanged, err := MetadataValuesEqual(plan.Expected, plan.Value)
	if err != nil {
		return RevisionTransitionPlan{}, fmt.Errorf("%w: compare marker values: %v", publicops.ErrValidation, err)
	}
	if unchanged {
		return RevisionTransitionPlan{}, fmt.Errorf("%w: revision transition must change the metadata marker", publicops.ErrValidation)
	}
	payload := in.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	if plan.Payload, err = CanonicalMetadataValue(payload); err != nil {
		return RevisionTransitionPlan{}, fmt.Errorf("%w: receipt payload: %v", publicops.ErrValidation, err)
	}
	return plan, nil
}

// ValidateRevisionTransitionReceiptID checks the exact byte length and text
// shape used by the receipt table's binary primary key.
func ValidateRevisionTransitionReceiptID(id string) error {
	if id == "" || len(id) > 191 || !utf8.ValidString(id) || strings.ContainsRune(id, '\x00') {
		return fmt.Errorf("%w: receipt id must be valid nonempty text of at most 191 bytes", publicops.ErrValidation)
	}
	return nil
}

// SameRevisionTransitionRequest reports whether a durable receipt was created
// for exactly this request. The server-minted ToVersion is deliberately not
// part of the comparison.
func SameRevisionTransitionRequest(plan RevisionTransitionPlan, receipt *publicops.RevisionTransitionReceipt) bool {
	if receipt == nil || receipt.ReceiptID != plan.ReceiptID || receipt.IssueID != plan.IssueID ||
		receipt.Scope != plan.Scope || receipt.Kind != plan.Kind || receipt.Actor != plan.Actor ||
		receipt.ExpectedVersion != plan.ExpectedVersion || receipt.Key != plan.Key ||
		!bytesEqualOptional(receipt.Expected, plan.Expected) || !bytesEqualOptional(receipt.Value, plan.Value) {
		return false
	}
	return string(receipt.Payload) == string(plan.Payload)
}

func bytesEqualOptional(a, b *json.RawMessage) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return string(*a) == string(*b)
}
