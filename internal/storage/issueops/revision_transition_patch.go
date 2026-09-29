package issueops

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
	publicops "github.com/steveyegge/beads/issueops"
)

const revisionTransitionPatchReceiptType = "beads.issue-patch-transition.v1"

type revisionTransitionPatchEnvelope struct {
	Type    string          `json:"type"`
	Request json.RawMessage `json:"request"`
	Receipt json.RawMessage `json:"receipt"`
}

// PreflightRevisionTransitionPatchInTx returns an exact durable replay without
// entering a write transaction. It refuses cross-kind ID reuse, unsupported
// source planes, and missing or mismatched parent receipts.
func PreflightRevisionTransitionPatchInTx(ctx context.Context, tx DBTX, plan storage.RevisionTransitionPatchPlan) (publicops.RevisionTransitionPatchResult, bool, error) {
	receipt, err := getRevisionTransitionPatchReceiptInTx(ctx, tx, plan.ReceiptID)
	if err == nil {
		if !storage.SameRevisionTransitionPatchRequest(plan, receipt) {
			return publicops.RevisionTransitionPatchResult{}, false, fmt.Errorf("%w: receipt %s is bound to another request", publicops.ErrTransitionReceiptConflict, plan.ReceiptID)
		}
		return publicops.RevisionTransitionPatchResult{Applied: true, Replayed: true, Receipt: receipt}, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return publicops.RevisionTransitionPatchResult{}, false, err
	}
	if _, err := getRevisionTransitionReceiptInTx(ctx, tx, plan.ReceiptID); err == nil {
		return publicops.RevisionTransitionPatchResult{}, false, fmt.Errorf("%w: receipt %s is bound to a legacy metadata request", publicops.ErrTransitionReceiptConflict, plan.ReceiptID)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return publicops.RevisionTransitionPatchResult{}, false, err
	}
	if err := ValidateRevisionTransitionSourceInTx(ctx, tx, plan.IssueID); err != nil {
		return publicops.RevisionTransitionPatchResult{}, false, err
	}
	if err := validateRevisionTransitionPatchParentInTx(ctx, tx, plan); err != nil {
		return publicops.RevisionTransitionPatchResult{}, false, err
	}
	return publicops.RevisionTransitionPatchResult{}, false, nil
}

// TransitionPatchInTx atomically verifies a source row revision, the exact
// parent-receipt chain, and every expected field before applying the full
// patch. Its canonical request, exact receipt, issue update, labels, event,
// and machine journal record share the caller's transaction.
func TransitionPatchInTx(ctx context.Context, tx DBTX, plan storage.RevisionTransitionPatchPlan) (publicops.RevisionTransitionPatchResult, RevisionTransitionWrite, error) {
	preflight, done, err := PreflightRevisionTransitionPatchInTx(ctx, tx, plan)
	if err != nil || done {
		return preflight, RevisionTransitionWrite{}, err
	}
	current, currentVersion, currentMetadata, currentLabels, err := readRevisionTransitionPatchSourceInTx(ctx, tx, plan.IssueID)
	if err != nil {
		return publicops.RevisionTransitionPatchResult{}, RevisionTransitionWrite{}, err
	}
	if currentVersion != plan.ExpectedVersion {
		return publicops.RevisionTransitionPatchResult{}, RevisionTransitionWrite{}, fmt.Errorf("%w: expected %d, got %d", storage.ErrVersionMismatch, plan.ExpectedVersion, currentVersion)
	}
	if err := validateRevisionTransitionPatchExpected(current, currentMetadata, currentLabels, plan.Patch); err != nil {
		return publicops.RevisionTransitionPatchResult{}, RevisionTransitionWrite{}, err
	}

	updates := make(map[string]interface{}, 3)
	if len(plan.Patch.Metadata) > 0 {
		for _, entry := range plan.Patch.Metadata {
			if entry.Value == nil {
				delete(currentMetadata, entry.Key)
			} else {
				currentMetadata[entry.Key] = append(json.RawMessage(nil), (*entry.Value)...)
			}
		}
		merged, err := json.Marshal(currentMetadata)
		if err != nil {
			return publicops.RevisionTransitionPatchResult{}, RevisionTransitionWrite{}, fmt.Errorf("marshal patched metadata for %s: %w", plan.IssueID, err)
		}
		if err := ValidateMetadataIfConfigured(merged); err != nil {
			return publicops.RevisionTransitionPatchResult{}, RevisionTransitionWrite{}, err
		}
		updates["metadata"] = string(merged)
	}
	if plan.Patch.Status != nil {
		customStatuses, err := ResolveCustomStatusesDetailedInTx(ctx, tx)
		if err != nil {
			return publicops.RevisionTransitionPatchResult{}, RevisionTransitionWrite{}, fmt.Errorf("resolve custom statuses for transition patch: %w", err)
		}
		if !types.Status(plan.Patch.Status.Value).IsValidWithCustomStatuses(customStatuses) {
			return publicops.RevisionTransitionPatchResult{}, RevisionTransitionWrite{}, fmt.Errorf("%w: invalid status %q", storage.ErrValidation, plan.Patch.Status.Value)
		}
		updates["status"] = plan.Patch.Status.Value
	}
	if plan.Patch.Assignee != nil {
		updates["assignee"] = plan.Patch.Assignee.Value
	}

	toVersion := freshRowLock()
	for toVersion == 0 || toVersion == plan.ExpectedVersion {
		toVersion = freshRowLock()
	}
	receipt := revisionTransitionPatchReceiptFromPlan(plan, toVersion)
	requestJSON, err := json.Marshal(plan)
	if err != nil {
		return publicops.RevisionTransitionPatchResult{}, RevisionTransitionWrite{}, fmt.Errorf("encode canonical patch request %s: %w", plan.ReceiptID, err)
	}
	receiptJSON, err := json.Marshal(receipt)
	if err != nil {
		return publicops.RevisionTransitionPatchResult{}, RevisionTransitionWrite{}, fmt.Errorf("encode canonical patch receipt %s: %w", plan.ReceiptID, err)
	}
	envelopeJSON, err := json.Marshal(revisionTransitionPatchEnvelope{
		Type: revisionTransitionPatchReceiptType, Request: requestJSON, Receipt: receiptJSON,
	})
	if err != nil {
		return publicops.RevisionTransitionPatchResult{}, RevisionTransitionWrite{}, fmt.Errorf("encode durable patch receipt envelope %s: %w", plan.ReceiptID, err)
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO issue_transition_receipts
			(receipt_id, issue_id, scope, kind, actor, metadata_key,
			 from_revision, to_revision, expected_present, expected_json,
			 value_present, value_json, payload_json, request_json, receipt_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, NULL, 0, NULL, ?, ?, ?)
	`, plan.ReceiptID, plan.IssueID, plan.Scope, plan.Kind, plan.Actor,
		storage.RevisionTransitionPatchReceiptMarker, plan.ExpectedVersion, toVersion,
		string(envelopeJSON), string(requestJSON), string(receiptJSON))
	if err != nil {
		return publicops.RevisionTransitionPatchResult{}, RevisionTransitionWrite{}, classifyRevisionTransitionReceiptInsertError(plan.ReceiptID, err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return publicops.RevisionTransitionPatchResult{}, RevisionTransitionWrite{}, fmt.Errorf("insert patch transition receipt %s: rows affected: %w", plan.ReceiptID, err)
	}
	if inserted != 1 {
		return publicops.RevisionTransitionPatchResult{}, RevisionTransitionWrite{}, fmt.Errorf("insert patch transition receipt %s: inserted %d rows, want 1", plan.ReceiptID, inserted)
	}

	var exactLabels *[]string
	if plan.Patch.Labels != nil {
		labels := append([]string(nil), plan.Patch.Labels.Value...)
		exactLabels = &labels
	}
	updated, err := updateIssueWithRowLockAndExactLabelsInTx(ctx, tx, plan.IssueID, updates, exactLabels, plan.Actor, toVersion)
	if err != nil {
		return publicops.RevisionTransitionPatchResult{}, RevisionTransitionWrite{}, err
	}
	if updated == nil || !updated.Changed || !updated.IssueRowsChanged {
		return publicops.RevisionTransitionPatchResult{}, RevisionTransitionWrite{}, fmt.Errorf("revision transition patch for %s did not update its durable issue row", plan.IssueID)
	}
	var persistedVersion sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT row_lock FROM issues WHERE id = ?`, plan.IssueID).Scan(&persistedVersion); err != nil {
		return publicops.RevisionTransitionPatchResult{}, RevisionTransitionWrite{}, fmt.Errorf("read resulting issue revision for %s: %w", plan.IssueID, err)
	}
	if !persistedVersion.Valid || persistedVersion.Int64 != toVersion {
		return publicops.RevisionTransitionPatchResult{}, RevisionTransitionWrite{}, fmt.Errorf("revision transition patch for %s wrote row_lock %d, receipt reserved %d", plan.IssueID, persistedVersion.Int64, toVersion)
	}
	stored, err := getRevisionTransitionPatchReceiptInTx(ctx, tx, plan.ReceiptID)
	if err != nil || !storage.SameRevisionTransitionPatchRequest(plan, stored) || stored.ToVersion != toVersion {
		if err != nil {
			return publicops.RevisionTransitionPatchResult{}, RevisionTransitionWrite{}, fmt.Errorf("verify stored patch receipt %s: %w", plan.ReceiptID, err)
		}
		return publicops.RevisionTransitionPatchResult{}, RevisionTransitionWrite{}, fmt.Errorf("stored patch receipt %s differs from the transaction request", plan.ReceiptID)
	}
	write := RevisionTransitionWrite{Wrote: true, Tables: ChangedTables{}}
	write.Tables.Add("issues", "events", "issue_transition_receipts")
	return publicops.RevisionTransitionPatchResult{Applied: true, Receipt: stored}, write, nil
}

func readRevisionTransitionPatchSourceInTx(ctx context.Context, tx DBTX, issueID string) (*types.Issue, int64, map[string]json.RawMessage, []string, error) {
	var version int64
	var status string
	var assignee sql.NullString
	var metadata sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT row_lock, status, assignee, metadata FROM issues WHERE id = ?`, issueID).
		Scan(&version, &status, &assignee, &metadata); errors.Is(err, sql.ErrNoRows) {
		return nil, 0, nil, nil, fmt.Errorf("%w: issue %s", storage.ErrNotFound, issueID)
	} else if err != nil {
		return nil, 0, nil, nil, fmt.Errorf("read revision patch source %s: %w", issueID, err)
	}
	metadataMap := make(map[string]json.RawMessage)
	if metadata.Valid && metadata.String != "" && metadata.String != "null" {
		if err := json.Unmarshal([]byte(metadata.String), &metadataMap); err != nil {
			return nil, 0, nil, nil, fmt.Errorf("parse revision patch metadata for %s: %w", issueID, err)
		}
	}
	labels, err := GetLabelsInTx(ctx, tx, "labels", issueID)
	if err != nil {
		return nil, 0, nil, nil, fmt.Errorf("read revision patch labels for %s: %w", issueID, err)
	}
	issue := &types.Issue{ID: issueID, Status: types.Status(status), Assignee: assignee.String, Labels: labels}
	return issue, version, metadataMap, labels, nil
}

func validateRevisionTransitionPatchExpected(current *types.Issue, metadata map[string]json.RawMessage, labels []string, patch publicops.RevisionTransitionIssuePatch) error {
	for _, entry := range patch.Metadata {
		actual, err := canonicalMetadataKey(metadata, entry.Key)
		if err != nil {
			return fmt.Errorf("metadata key %q: %w", entry.Key, err)
		}
		if !sameCanonicalValue(actual, entry.Expected) {
			return fmt.Errorf("%w: metadata key %q changed", publicops.ErrRevisionPatchPreconditionFailed, entry.Key)
		}
	}
	if patch.Status != nil && string(current.Status) != patch.Status.Expected {
		return fmt.Errorf("%w: status changed", publicops.ErrRevisionPatchPreconditionFailed)
	}
	if patch.Assignee != nil && current.Assignee != patch.Assignee.Expected {
		return fmt.Errorf("%w: assignee changed", publicops.ErrRevisionPatchPreconditionFailed)
	}
	if patch.Labels != nil && !sameIssueLabels(labels, patch.Labels.Expected) {
		return fmt.Errorf("%w: labels changed", publicops.ErrRevisionPatchPreconditionFailed)
	}
	return nil
}

func validateRevisionTransitionPatchParentInTx(ctx context.Context, tx DBTX, plan storage.RevisionTransitionPatchPlan) error {
	var parentIssueID, parentScope string
	var parentVersion int64
	var digest string
	parent, err := getRevisionTransitionReceiptInTx(ctx, tx, plan.PriorReceiptID)
	if err == nil {
		parentIssueID, parentScope, parentVersion = parent.IssueID, parent.Scope, parent.ToVersion
		digest, err = publicops.RevisionTransitionReceiptDigest(*parent)
	} else if errors.Is(err, publicops.ErrTransitionReceiptConflict) {
		var patchParent *publicops.RevisionTransitionPatchReceipt
		patchParent, err = getRevisionTransitionPatchReceiptInTx(ctx, tx, plan.PriorReceiptID)
		if err == nil {
			parentIssueID, parentScope, parentVersion = patchParent.IssueID, patchParent.Scope, patchParent.ToVersion
			digest, err = publicops.RevisionTransitionPatchReceiptDigest(*patchParent)
		}
	}
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, storage.ErrNotFound) || errors.Is(err, publicops.ErrTransitionReceiptConflict) {
			return fmt.Errorf("%w: prior receipt %s is absent or has another receipt type", publicops.ErrTransitionReceiptConflict, plan.PriorReceiptID)
		}
		return fmt.Errorf("read parent receipt %s: %w", plan.PriorReceiptID, err)
	}
	if parentIssueID != plan.IssueID || parentScope != plan.Scope || parentVersion != plan.ExpectedVersion || digest != plan.PriorReceiptDigest {
		return fmt.Errorf("%w: prior receipt %s does not bind this issue, scope, revision, and digest", publicops.ErrTransitionReceiptConflict, plan.PriorReceiptID)
	}
	return nil
}

func getRevisionTransitionPatchReceiptInTx(ctx context.Context, tx DBTX, receiptID string) (*publicops.RevisionTransitionPatchReceipt, error) {
	var issueID, scope, kind, actor, marker, payload string
	var fromVersion, toVersion int64
	var expectedPresent, valuePresent int
	var expectedJSON, valueJSON, requestJSON, receiptJSON sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT receipt_id, issue_id, scope, kind, actor, metadata_key,
		       from_revision, to_revision, expected_present, expected_json,
		       value_present, value_json, payload_json, request_json, receipt_json
		FROM issue_transition_receipts WHERE receipt_id = ?
	`, receiptID).Scan(&receiptID, &issueID, &scope, &kind, &actor, &marker,
		&fromVersion, &toVersion, &expectedPresent, &expectedJSON, &valuePresent,
		&valueJSON, &payload, &requestJSON, &receiptJSON)
	if err != nil {
		return nil, err
	}
	if marker != storage.RevisionTransitionPatchReceiptMarker {
		return nil, fmt.Errorf("%w: receipt %s is a legacy metadata transition", publicops.ErrTransitionReceiptConflict, receiptID)
	}
	if expectedPresent != 0 || expectedJSON.Valid || valuePresent != 0 || valueJSON.Valid {
		return nil, fmt.Errorf("patch transition receipt %s has unexpected legacy marker fields", receiptID)
	}
	if fromVersion == 0 || toVersion == 0 || fromVersion == toVersion {
		return nil, fmt.Errorf("patch transition receipt %s has invalid revision tokens", receiptID)
	}
	var envelope revisionTransitionPatchEnvelope
	if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
		return nil, fmt.Errorf("decode patch transition receipt %s envelope: %w", receiptID, err)
	}
	if envelope.Type != revisionTransitionPatchReceiptType || len(envelope.Request) == 0 || len(envelope.Receipt) == 0 {
		return nil, fmt.Errorf("patch transition receipt %s has an incomplete envelope", receiptID)
	}
	var request publicops.RevisionTransitionPatchRequest
	if err := json.Unmarshal(envelope.Request, &request); err != nil {
		return nil, fmt.Errorf("decode patch transition receipt %s request: %w", receiptID, err)
	}
	plan, err := storage.PlanRevisionTransitionPatch(request)
	if err != nil {
		return nil, fmt.Errorf("patch transition receipt %s violates request invariants: %w", receiptID, err)
	}
	canonicalRequest, err := json.Marshal(plan)
	if err != nil || string(canonicalRequest) != string(envelope.Request) {
		return nil, fmt.Errorf("patch transition receipt %s request is not canonical", receiptID)
	}
	var receipt publicops.RevisionTransitionPatchReceipt
	if err := json.Unmarshal(envelope.Receipt, &receipt); err != nil {
		return nil, fmt.Errorf("decode patch transition receipt %s result: %w", receiptID, err)
	}
	canonicalReceipt := revisionTransitionPatchReceiptFromPlan(plan, toVersion)
	canonicalReceiptJSON, err := json.Marshal(canonicalReceipt)
	if err != nil || string(canonicalReceiptJSON) != string(envelope.Receipt) ||
		!storage.SameRevisionTransitionPatchRequest(plan, &receipt) || receipt.ToVersion != toVersion {
		return nil, fmt.Errorf("patch transition receipt %s result is not canonically bound to its request", receiptID)
	}
	canonicalEnvelope, err := json.Marshal(revisionTransitionPatchEnvelope{
		Type: revisionTransitionPatchReceiptType, Request: canonicalRequest, Receipt: canonicalReceiptJSON,
	})
	if err != nil || string(canonicalEnvelope) != payload {
		return nil, fmt.Errorf("patch transition receipt %s envelope is not canonical", receiptID)
	}
	if requestJSON.Valid != receiptJSON.Valid {
		return nil, fmt.Errorf("patch transition receipt %s has an incomplete indexed request binding", receiptID)
	}
	if requestJSON.Valid && (requestJSON.String != string(canonicalRequest) || receiptJSON.String != string(canonicalReceiptJSON)) {
		return nil, fmt.Errorf("patch transition receipt %s indexed request binding is inconsistent", receiptID)
	}
	if receipt.ReceiptID != receiptID || receipt.IssueID != issueID || receipt.Scope != scope || receipt.Kind != kind || receipt.Actor != actor ||
		receipt.ExpectedVersion != fromVersion || receipt.ToVersion != toVersion {
		return nil, fmt.Errorf("patch transition receipt %s normalized columns disagree with its body", receiptID)
	}
	return &receipt, nil
}

// GetRevisionTransitionPatchReceiptInTx reads one exact durable source patch
// receipt by ID. It is the read used for lost-response recovery and chaining.
func GetRevisionTransitionPatchReceiptInTx(ctx context.Context, tx DBTX, receiptID string) (*publicops.RevisionTransitionPatchReceipt, error) {
	if err := storage.ValidateRevisionTransitionReceiptID(receiptID); err != nil {
		return nil, err
	}
	receipt, err := getRevisionTransitionPatchReceiptInTx(ctx, tx, receiptID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: patch transition receipt %s", storage.ErrNotFound, receiptID)
	}
	return receipt, err
}

func revisionTransitionPatchReceiptFromPlan(plan storage.RevisionTransitionPatchPlan, toVersion int64) *publicops.RevisionTransitionPatchReceipt {
	return &publicops.RevisionTransitionPatchReceipt{
		ReceiptID: plan.ReceiptID, IssueID: plan.IssueID, Scope: plan.Scope,
		Kind: plan.Kind, Actor: plan.Actor, ExpectedVersion: plan.ExpectedVersion,
		ToVersion: toVersion, PriorReceiptID: plan.PriorReceiptID,
		PriorReceiptDigest: plan.PriorReceiptDigest, Patch: plan.Patch,
	}
}
