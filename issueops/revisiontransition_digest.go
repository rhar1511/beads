package issueops

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
)

// RevisionTransitionReceiptDigest returns the canonical digest used when a
// later source patch names a metadata-transition receipt as its parent. It
// uses the receipt's wire field names and canonical decimal revision tokens so
// callers can compute the same value from a typed client receipt.
func RevisionTransitionReceiptDigest(receipt RevisionTransitionReceipt) (string, error) {
	type canonicalReceipt struct {
		ReceiptID       string          `json:"receipt_id"`
		IssueID         string          `json:"issue_id"`
		Scope           string          `json:"scope"`
		Kind            string          `json:"kind"`
		Actor           string          `json:"actor"`
		ExpectedVersion string          `json:"expected_version"`
		ToVersion       string          `json:"to_version"`
		Key             string          `json:"key"`
		Expected        json.RawMessage `json:"expected,omitempty"`
		Value           json.RawMessage `json:"value,omitempty"`
		Payload         json.RawMessage `json:"payload"`
	}
	canonical := canonicalReceipt{
		ReceiptID: receipt.ReceiptID, IssueID: receipt.IssueID, Scope: receipt.Scope,
		Kind: receipt.Kind, Actor: receipt.Actor,
		ExpectedVersion: strconv.FormatInt(receipt.ExpectedVersion, 10),
		ToVersion:       strconv.FormatInt(receipt.ToVersion, 10), Key: receipt.Key,
		Payload: append(json.RawMessage(nil), receipt.Payload...),
	}
	if receipt.Expected != nil {
		canonical.Expected = append(json.RawMessage(nil), (*receipt.Expected)...)
	}
	if receipt.Value != nil {
		canonical.Value = append(json.RawMessage(nil), (*receipt.Value)...)
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

// RevisionTransitionPatchReceiptDigest returns the canonical digest of an
// immutable patch receipt for use as the parent binding on the next source
// mutation.
func RevisionTransitionPatchReceiptDigest(receipt RevisionTransitionPatchReceipt) (string, error) {
	type canonicalReceipt struct {
		ReceiptID          string                       `json:"receipt_id"`
		IssueID            string                       `json:"issue_id"`
		Scope              string                       `json:"scope"`
		Kind               string                       `json:"kind"`
		Actor              string                       `json:"actor"`
		ExpectedVersion    string                       `json:"expected_version"`
		ToVersion          string                       `json:"to_version"`
		PriorReceiptID     string                       `json:"prior_receipt_id"`
		PriorReceiptDigest string                       `json:"prior_receipt_digest"`
		Patch              RevisionTransitionIssuePatch `json:"patch"`
	}
	canonical := canonicalReceipt{
		ReceiptID: receipt.ReceiptID, IssueID: receipt.IssueID, Scope: receipt.Scope,
		Kind: receipt.Kind, Actor: receipt.Actor,
		ExpectedVersion: strconv.FormatInt(receipt.ExpectedVersion, 10),
		ToVersion:       strconv.FormatInt(receipt.ToVersion, 10),
		PriorReceiptID:  receipt.PriorReceiptID, PriorReceiptDigest: receipt.PriorReceiptDigest,
		Patch: receipt.Patch,
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}
