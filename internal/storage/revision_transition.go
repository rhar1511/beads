package storage

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	publicops "github.com/rhar1511/beads/issueops"
)

// RevisionTransitionPatchReceiptMarker occupies the legacy metadata_key column
// for typed patch receipts so both request types share the table's primary
// receipt-ID constraint without changing the one-key receipt fields.
const RevisionTransitionPatchReceiptMarker = "__beads_atomic_issue_patch_v1__"

// RevisionTransitionPatchPlan is the validated, canonical patch request used
// by the durable transaction. All caller-owned slices and JSON values are
// copied before this plan is returned.
type RevisionTransitionPatchPlan struct {
	ReceiptID          string                                 `json:"receipt_id"`
	IssueID            string                                 `json:"issue_id"`
	Scope              string                                 `json:"scope"`
	Kind               string                                 `json:"kind"`
	Actor              string                                 `json:"actor"`
	ExpectedVersion    int64                                  `json:"expected_version"`
	PriorReceiptID     string                                 `json:"prior_receipt_id"`
	PriorReceiptDigest string                                 `json:"prior_receipt_digest"`
	Patch              publicops.RevisionTransitionIssuePatch `json:"patch"`
}

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
	if in.Key == RevisionTransitionPatchReceiptMarker {
		return RevisionTransitionPlan{}, fmt.Errorf("%w: metadata key is reserved for typed transition receipts", publicops.ErrValidation)
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

// PlanRevisionTransitionPatch validates and canonicalizes a typed atomic
// source patch before any transaction or connection is opened.
func PlanRevisionTransitionPatch(in publicops.RevisionTransitionPatchRequest) (RevisionTransitionPatchPlan, error) {
	if err := ValidateRevisionTransitionReceiptID(in.ReceiptID); err != nil {
		return RevisionTransitionPatchPlan{}, err
	}
	if err := ValidateRevisionTransitionReceiptID(in.PriorReceiptID); err != nil {
		return RevisionTransitionPatchPlan{}, fmt.Errorf("%w: prior receipt ID: %v", publicops.ErrValidation, err)
	}
	validateText := func(name, value string, max int) error {
		if value == "" || !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("%w: revision transition patch requires valid %s text", publicops.ErrValidation, name)
		}
		if utf8.RuneCountInString(value) > max {
			return fmt.Errorf("%w: revision transition patch %s exceeds %d characters", publicops.ErrValidation, name, max)
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
		{"prior receipt id", in.PriorReceiptID, 191},
	} {
		if err := validateText(field.name, field.value, field.max); err != nil {
			return RevisionTransitionPatchPlan{}, err
		}
	}
	if in.ExpectedVersion == 0 {
		return RevisionTransitionPatchPlan{}, fmt.Errorf("%w: expected issue revision must be nonzero", publicops.ErrValidation)
	}
	if len(in.PriorReceiptDigest) != 64 || strings.ToLower(in.PriorReceiptDigest) != in.PriorReceiptDigest {
		return RevisionTransitionPatchPlan{}, fmt.Errorf("%w: prior receipt digest must be 64 lowercase hexadecimal characters", publicops.ErrValidation)
	}
	if _, err := hex.DecodeString(in.PriorReceiptDigest); err != nil {
		return RevisionTransitionPatchPlan{}, fmt.Errorf("%w: prior receipt digest must be 64 lowercase hexadecimal characters", publicops.ErrValidation)
	}

	plan := RevisionTransitionPatchPlan{
		ReceiptID: in.ReceiptID, IssueID: in.IssueID, Scope: in.Scope,
		Kind: in.Kind, Actor: in.Actor, ExpectedVersion: in.ExpectedVersion,
		PriorReceiptID: in.PriorReceiptID, PriorReceiptDigest: in.PriorReceiptDigest,
		Patch: publicops.RevisionTransitionIssuePatch{},
	}
	seenKeys := make(map[string]struct{}, len(in.Patch.Metadata))
	for _, item := range in.Patch.Metadata {
		if err := ValidateMetadataKey(item.Key); err != nil {
			return RevisionTransitionPatchPlan{}, fmt.Errorf("%w: invalid patch metadata key: %v", publicops.ErrValidation, err)
		}
		if err := validateText("metadata key", item.Key, 255); err != nil {
			return RevisionTransitionPatchPlan{}, err
		}
		if _, duplicate := seenKeys[item.Key]; duplicate {
			return RevisionTransitionPatchPlan{}, fmt.Errorf("%w: duplicate patch metadata key %q", publicops.ErrValidation, item.Key)
		}
		seenKeys[item.Key] = struct{}{}
		expected, err := CanonicalMetadataPointer(item.Expected)
		if err != nil {
			return RevisionTransitionPatchPlan{}, fmt.Errorf("%w: metadata key %q expected value: %v", publicops.ErrValidation, item.Key, err)
		}
		value, err := CanonicalMetadataPointer(item.Value)
		if err != nil {
			return RevisionTransitionPatchPlan{}, fmt.Errorf("%w: metadata key %q next value: %v", publicops.ErrValidation, item.Key, err)
		}
		unchanged, err := MetadataValuesEqual(expected, value)
		if err != nil {
			return RevisionTransitionPatchPlan{}, fmt.Errorf("%w: compare patch metadata key %q: %v", publicops.ErrValidation, item.Key, err)
		}
		if unchanged {
			return RevisionTransitionPatchPlan{}, fmt.Errorf("%w: patch metadata key %q must change", publicops.ErrValidation, item.Key)
		}
		plan.Patch.Metadata = append(plan.Patch.Metadata, publicops.RevisionTransitionMetadataPatch{
			Key: item.Key, Expected: expected, Value: value,
		})
	}
	sort.Slice(plan.Patch.Metadata, func(i, j int) bool {
		return plan.Patch.Metadata[i].Key < plan.Patch.Metadata[j].Key
	})

	if in.Patch.Status != nil {
		status := *in.Patch.Status
		if err := validateText("expected status", status.Expected, 32); err != nil {
			return RevisionTransitionPatchPlan{}, err
		}
		if err := validateText("next status", status.Value, 32); err != nil {
			return RevisionTransitionPatchPlan{}, err
		}
		if status.Expected == status.Value {
			return RevisionTransitionPatchPlan{}, fmt.Errorf("%w: patch status must change", publicops.ErrValidation)
		}
		plan.Patch.Status = &status
	}
	if in.Patch.Assignee != nil {
		assignee := *in.Patch.Assignee
		if !utf8.ValidString(assignee.Expected) || strings.ContainsRune(assignee.Expected, '\x00') || utf8.RuneCountInString(assignee.Expected) > 255 ||
			!utf8.ValidString(assignee.Value) || strings.ContainsRune(assignee.Value, '\x00') || utf8.RuneCountInString(assignee.Value) > 255 {
			return RevisionTransitionPatchPlan{}, fmt.Errorf("%w: patch assignee values must be valid text of at most 255 characters", publicops.ErrValidation)
		}
		if assignee.Expected == assignee.Value {
			return RevisionTransitionPatchPlan{}, fmt.Errorf("%w: patch assignee must change", publicops.ErrValidation)
		}
		plan.Patch.Assignee = &assignee
	}
	if in.Patch.Labels != nil {
		labels := publicops.RevisionTransitionLabelsPatch{
			Expected: append([]string{}, in.Patch.Labels.Expected...),
			Value:    append([]string{}, in.Patch.Labels.Value...),
		}
		if err := canonicalPatchLabels(labels.Expected); err != nil {
			return RevisionTransitionPatchPlan{}, fmt.Errorf("%w: expected labels: %v", publicops.ErrValidation, err)
		}
		if err := canonicalPatchLabels(labels.Value); err != nil {
			return RevisionTransitionPatchPlan{}, fmt.Errorf("%w: next labels: %v", publicops.ErrValidation, err)
		}
		if equalStrings(labels.Expected, labels.Value) {
			return RevisionTransitionPatchPlan{}, fmt.Errorf("%w: patch labels must change", publicops.ErrValidation)
		}
		plan.Patch.Labels = &labels
	}
	if len(plan.Patch.Metadata) == 0 && plan.Patch.Status == nil && plan.Patch.Assignee == nil && plan.Patch.Labels == nil {
		return RevisionTransitionPatchPlan{}, fmt.Errorf("%w: revision transition patch must change at least one field", publicops.ErrValidation)
	}
	return plan, nil
}

func canonicalPatchLabels(labels []string) error {
	for _, label := range labels {
		if label == "" || !utf8.ValidString(label) || strings.ContainsRune(label, '\x00') || len(label) > 255 {
			return fmt.Errorf("labels must be unique valid nonempty text of at most 255 bytes")
		}
	}
	sort.Strings(labels)
	for i := 1; i < len(labels); i++ {
		if labels[i] == labels[i-1] {
			return fmt.Errorf("duplicate label %q", labels[i])
		}
	}
	return nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// SameRevisionTransitionPatchRequest compares the complete canonical request
// against an immutable durable receipt. Server-minted ToVersion is excluded.
func SameRevisionTransitionPatchRequest(plan RevisionTransitionPatchPlan, receipt *publicops.RevisionTransitionPatchReceipt) bool {
	if receipt == nil {
		return false
	}
	receiptPlan, err := PlanRevisionTransitionPatch(publicops.RevisionTransitionPatchRequest{
		ReceiptID: receipt.ReceiptID, IssueID: receipt.IssueID, Scope: receipt.Scope,
		Kind: receipt.Kind, Actor: receipt.Actor, ExpectedVersion: receipt.ExpectedVersion,
		PriorReceiptID: receipt.PriorReceiptID, PriorReceiptDigest: receipt.PriorReceiptDigest,
		Patch: receipt.Patch,
	})
	if err != nil {
		return false
	}
	left, err := json.Marshal(plan)
	if err != nil {
		return false
	}
	right, err := json.Marshal(receiptPlan)
	return err == nil && string(left) == string(right)
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
