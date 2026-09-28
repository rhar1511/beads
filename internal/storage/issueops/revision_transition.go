package issueops

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/dberrors"
	publicops "github.com/steveyegge/beads/issueops"
)

// ValidateRevisionTransitionSourceInTx refuses transition snapshots that
// depend on the dolt-ignored wisp plane. The receipt is versioned with the
// durable issues table, so its source and outgoing edges must also be durable.
// Call it in a read-only preflight before opening the write transaction, then
// repeat it inside the write transaction to close the gap between those
// transactions.
func ValidateRevisionTransitionSourceInTx(ctx context.Context, tx DBTX, issueID string) error {
	var found int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM wisps WHERE id = ? LIMIT 1`, issueID).Scan(&found)
	switch {
	case err == nil:
		return revisionTransitionWispUnsupported("source issue is stored in wisps")
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return fmt.Errorf("check revision transition source plane for %s: %w", issueID, err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM issues WHERE id = ? LIMIT 1`, issueID).Scan(&found); errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: issue %s", storage.ErrNotFound, issueID)
	} else if err != nil {
		return fmt.Errorf("check revision transition issue %s: %w", issueID, err)
	}
	for _, probe := range []struct {
		query string
		why   string
	}{
		{`SELECT 1 FROM wisp_dependencies WHERE issue_id = ? LIMIT 1`, "source has outgoing wisp dependency rows"},
		{`SELECT 1 FROM dependencies WHERE issue_id = ? AND depends_on_wisp_id IS NOT NULL LIMIT 1`, "source has a wisp-backed dependency target"},
	} {
		err := tx.QueryRowContext(ctx, probe.query, issueID).Scan(&found)
		if err == nil {
			return revisionTransitionWispUnsupported(probe.why)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check revision transition dependency plane for %s: %w", issueID, err)
		}
	}
	return nil
}

func revisionTransitionWispUnsupported(reason string) error {
	return fmt.Errorf("%w: %s", &storage.ErrUnsupported{Op: "RevisionTransition", Backend: "Dolt wisp plane"}, reason)
}

// PreflightRevisionTransitionInTx returns an exact durable retry without
// opening a write transaction. If the receipt is absent, it verifies that the
// current source and its outgoing edges stay on the versioned issue plane.
func PreflightRevisionTransitionInTx(ctx context.Context, tx DBTX, plan storage.RevisionTransitionPlan) (publicops.RevisionTransitionResult, bool, error) {
	receipt, err := getRevisionTransitionReceiptInTx(ctx, tx, plan.ReceiptID)
	if err == nil {
		if !storage.SameRevisionTransitionRequest(plan, receipt) {
			return publicops.RevisionTransitionResult{}, false, fmt.Errorf("%w: receipt %s is bound to another request", publicops.ErrTransitionReceiptConflict, plan.ReceiptID)
		}
		return publicops.RevisionTransitionResult{Applied: true, Replayed: true, Receipt: receipt}, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return publicops.RevisionTransitionResult{}, false, err
	}
	if err := ValidateRevisionTransitionSourceInTx(ctx, tx, plan.IssueID); err != nil {
		return publicops.RevisionTransitionResult{}, false, err
	}
	return publicops.RevisionTransitionResult{}, false, nil
}

// RevisionTransitionWrite reports the durable tables changed by one applied
// transition. A marker refusal and an exact receipt replay write nothing.
type RevisionTransitionWrite struct {
	Wrote  bool
	Tables ChangedTables
}

// ErrRevisionTransitionReceiptRace tells a role wrapper to reconcile a
// duplicate receipt insert in a fresh transaction, where the winner's commit
// is visible to the transaction snapshot.
var ErrRevisionTransitionReceiptRace = errors.New("revision transition receipt identity raced")

// TransitionMetadataInTx atomically compares the permanent issue revision and
// metadata marker, applies the marker, and stores the actual resulting row_lock
// in an immutable receipt. The caller owns transaction and version-control
// commit boundaries.
func TransitionMetadataInTx(ctx context.Context, tx DBTX, plan storage.RevisionTransitionPlan) (publicops.RevisionTransitionResult, RevisionTransitionWrite, error) {
	preflight, done, err := PreflightRevisionTransitionInTx(ctx, tx, plan)
	if err != nil || done {
		return preflight, RevisionTransitionWrite{}, err
	}

	var currentVersion sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT row_lock FROM issues WHERE id = ?`, plan.IssueID).Scan(&currentVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return publicops.RevisionTransitionResult{}, RevisionTransitionWrite{}, fmt.Errorf("%w: issue %s", storage.ErrNotFound, plan.IssueID)
	}
	if err != nil {
		return publicops.RevisionTransitionResult{}, RevisionTransitionWrite{}, fmt.Errorf("read issue revision for %s: %w", plan.IssueID, err)
	}
	if currentVersion.Int64 != plan.ExpectedVersion {
		return publicops.RevisionTransitionResult{}, RevisionTransitionWrite{}, fmt.Errorf("%w: expected %d, got %d", storage.ErrVersionMismatch, plan.ExpectedVersion, currentVersion.Int64)
	}

	metadata, err := readMetadataMapInTx(ctx, tx, plan.IssueID)
	if err != nil {
		return publicops.RevisionTransitionResult{}, RevisionTransitionWrite{}, err
	}
	current, err := canonicalMetadataKey(metadata, plan.Key)
	if err != nil {
		return publicops.RevisionTransitionResult{}, RevisionTransitionWrite{}, fmt.Errorf("metadata key %q on %s: %w", plan.Key, plan.IssueID, err)
	}
	if !sameCanonicalValue(current, plan.Expected) {
		return publicops.RevisionTransitionResult{Current: cloneRawMessagePointer(current)}, RevisionTransitionWrite{}, nil
	}

	if plan.Value == nil {
		delete(metadata, plan.Key)
	} else {
		metadata[plan.Key] = append(json.RawMessage(nil), (*plan.Value)...)
	}
	merged, err := json.Marshal(metadata)
	if err != nil {
		return publicops.RevisionTransitionResult{}, RevisionTransitionWrite{}, fmt.Errorf("marshal transition metadata for %s: %w", plan.IssueID, err)
	}
	if err := ValidateMetadataIfConfigured(json.RawMessage(merged)); err != nil {
		return publicops.RevisionTransitionResult{}, RevisionTransitionWrite{}, err
	}

	toVersion := freshRowLock()
	for toVersion == 0 || toVersion == plan.ExpectedVersion {
		toVersion = freshRowLock()
	}
	// Insert the complete immutable receipt before changing the issue. If
	// another request reserved this ID after our first lookup, the unique-key
	// race is surfaced before this transaction mutates the issue. The wrapper
	// retries in a fresh transaction and returns the winning receipt or a typed
	// request-identity conflict.
	res, err := tx.ExecContext(ctx, `
		INSERT INTO issue_transition_receipts
			(receipt_id, issue_id, scope, kind, actor, metadata_key,
			 from_revision, to_revision, expected_present, expected_json,
			 value_present, value_json, payload_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, plan.ReceiptID, plan.IssueID, plan.Scope, plan.Kind, plan.Actor, plan.Key,
		plan.ExpectedVersion, toVersion, optionalJSONPresent(plan.Expected), optionalJSONString(plan.Expected),
		optionalJSONPresent(plan.Value), optionalJSONString(plan.Value), string(plan.Payload))
	if err != nil {
		return publicops.RevisionTransitionResult{}, RevisionTransitionWrite{}, classifyRevisionTransitionReceiptInsertError(plan.ReceiptID, err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return publicops.RevisionTransitionResult{}, RevisionTransitionWrite{}, fmt.Errorf("insert revision transition receipt %s: rows affected: %w", plan.ReceiptID, err)
	}
	if inserted != 1 {
		return publicops.RevisionTransitionResult{}, RevisionTransitionWrite{}, fmt.Errorf("insert revision transition receipt %s: inserted %d rows, want 1", plan.ReceiptID, inserted)
	}
	updated, err := updateIssueWithRowLockInTx(ctx, tx, plan.IssueID,
		map[string]interface{}{"metadata": string(merged)}, plan.Actor, toVersion)
	if err != nil {
		return publicops.RevisionTransitionResult{}, RevisionTransitionWrite{}, err
	}
	if updated == nil || !updated.Changed || !updated.IssueRowsChanged {
		return publicops.RevisionTransitionResult{}, RevisionTransitionWrite{}, fmt.Errorf("revision transition for %s did not update its issue row", plan.IssueID)
	}

	// Read back inside the same transaction instead of echoing the proposed
	// value. This is the token persisted in the durable receipt and returned to
	// callers after commit.
	var toRevision sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT row_lock FROM issues WHERE id = ?`, plan.IssueID).Scan(&toRevision); err != nil {
		return publicops.RevisionTransitionResult{}, RevisionTransitionWrite{}, fmt.Errorf("read resulting issue revision for %s: %w", plan.IssueID, err)
	}
	if !toRevision.Valid || toRevision.Int64 == 0 || toRevision.Int64 == plan.ExpectedVersion {
		return publicops.RevisionTransitionResult{}, RevisionTransitionWrite{}, fmt.Errorf("revision transition for %s produced an invalid row_lock token", plan.IssueID)
	}
	if toRevision.Int64 != toVersion {
		return publicops.RevisionTransitionResult{}, RevisionTransitionWrite{}, fmt.Errorf("revision transition for %s wrote row_lock %d but receipt reserved %d", plan.IssueID, toRevision.Int64, toVersion)
	}

	receipt := revisionTransitionReceiptFromPlan(plan, toRevision.Int64)
	write := RevisionTransitionWrite{Wrote: true, Tables: ChangedTables{}}
	write.Tables.Add("issues", "events", "issue_transition_receipts")
	return publicops.RevisionTransitionResult{Applied: true, Receipt: receipt}, write, nil
}

// classifyRevisionTransitionReceiptInsertError is called only for the plain
// INSERT into the receipt table. Its only unique index is the receipt ID, so a
// duplicate-key result is precisely the concurrent receipt-identity race; all
// other insert errors retain their cause and are never retried as a replay.
func classifyRevisionTransitionReceiptInsertError(receiptID string, err error) error {
	if dberrors.IsDuplicateKey(err) {
		return fmt.Errorf("insert revision transition receipt %s: %w", receiptID, ErrRevisionTransitionReceiptRace)
	}
	return fmt.Errorf("insert revision transition receipt %s: %w", receiptID, err)
}

// GetRevisionTransitionReceiptInTx reads one durable receipt by its exact
// caller-generated identity. It is also the recovery read after an unknown
// response or process restart.
func GetRevisionTransitionReceiptInTx(ctx context.Context, tx DBTX, receiptID string) (*publicops.RevisionTransitionReceipt, error) {
	receipt, err := getRevisionTransitionReceiptInTx(ctx, tx, receiptID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: transition receipt %s", storage.ErrNotFound, receiptID)
	}
	return receipt, err
}

func getRevisionTransitionReceiptInTx(ctx context.Context, tx DBTX, receiptID string) (*publicops.RevisionTransitionReceipt, error) {
	var receipt publicops.RevisionTransitionReceipt
	var expectedPresent, valuePresent int
	var expectedJSON, valueJSON sql.NullString
	var payload string
	err := tx.QueryRowContext(ctx, `
		SELECT receipt_id, issue_id, scope, kind, actor, metadata_key,
		       from_revision, to_revision, expected_present, expected_json,
		       value_present, value_json, payload_json
		FROM issue_transition_receipts WHERE receipt_id = ?
	`, receiptID).Scan(&receipt.ReceiptID, &receipt.IssueID, &receipt.Scope, &receipt.Kind,
		&receipt.Actor, &receipt.Key, &receipt.ExpectedVersion, &receipt.ToVersion,
		&expectedPresent, &expectedJSON, &valuePresent, &valueJSON, &payload)
	if err != nil {
		return nil, err
	}
	if expectedPresent != 0 && expectedPresent != 1 {
		return nil, fmt.Errorf("revision transition receipt %s has invalid expected-presence flag", receiptID)
	}
	if valuePresent != 0 && valuePresent != 1 {
		return nil, fmt.Errorf("revision transition receipt %s has invalid value-presence flag", receiptID)
	}
	if expectedPresent != 0 {
		if !expectedJSON.Valid || !json.Valid([]byte(expectedJSON.String)) {
			return nil, fmt.Errorf("revision transition receipt %s has invalid expected JSON", receiptID)
		}
		value := json.RawMessage(expectedJSON.String)
		receipt.Expected = &value
	} else if expectedJSON.Valid {
		return nil, fmt.Errorf("revision transition receipt %s has unexpected expected JSON", receiptID)
	}
	if valuePresent != 0 {
		if !valueJSON.Valid || !json.Valid([]byte(valueJSON.String)) {
			return nil, fmt.Errorf("revision transition receipt %s has invalid value JSON", receiptID)
		}
		value := json.RawMessage(valueJSON.String)
		receipt.Value = &value
	} else if valueJSON.Valid {
		return nil, fmt.Errorf("revision transition receipt %s has unexpected value JSON", receiptID)
	}
	receipt.Payload = json.RawMessage(payload)
	if receipt.ExpectedVersion == 0 || receipt.ToVersion == 0 || receipt.ToVersion == receipt.ExpectedVersion {
		return nil, fmt.Errorf("revision transition receipt %s has invalid revision tokens", receiptID)
	}
	// Revalidate the durable request using the same boundary contract as a new
	// request. Then require byte-for-byte canonical JSON so a damaged receipt
	// cannot be treated as an authoritative idempotency result.
	plan, err := storage.PlanRevisionTransition(publicops.RevisionTransitionRequest{
		ReceiptID: receipt.ReceiptID, IssueID: receipt.IssueID, Scope: receipt.Scope,
		Kind: receipt.Kind, Actor: receipt.Actor, ExpectedVersion: receipt.ExpectedVersion,
		Key: receipt.Key, Expected: receipt.Expected, Value: receipt.Value, Payload: receipt.Payload,
	})
	if err != nil {
		return nil, fmt.Errorf("revision transition receipt %s violates request invariants: %w", receiptID, err)
	}
	if !sameCanonicalStoredValue(receipt.Expected, plan.Expected) ||
		!sameCanonicalStoredValue(receipt.Value, plan.Value) || string(receipt.Payload) != string(plan.Payload) {
		return nil, fmt.Errorf("revision transition receipt %s contains non-canonical JSON", receiptID)
	}
	return &receipt, nil
}

func sameCanonicalStoredValue(a, b *json.RawMessage) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return string(*a) == string(*b)
}

func revisionTransitionReceiptFromPlan(plan storage.RevisionTransitionPlan, toVersion int64) *publicops.RevisionTransitionReceipt {
	return &publicops.RevisionTransitionReceipt{
		ReceiptID:       plan.ReceiptID,
		IssueID:         plan.IssueID,
		Scope:           plan.Scope,
		Kind:            plan.Kind,
		Actor:           plan.Actor,
		ExpectedVersion: plan.ExpectedVersion,
		ToVersion:       toVersion,
		Key:             plan.Key,
		Expected:        cloneRawMessagePointer(plan.Expected),
		Value:           cloneRawMessagePointer(plan.Value),
		Payload:         append(json.RawMessage(nil), plan.Payload...),
	}
}

func optionalJSONPresent(value *json.RawMessage) bool {
	return value != nil
}

func optionalJSONString(value *json.RawMessage) any {
	if value == nil {
		return nil
	}
	return string(*value)
}

func cloneRawMessagePointer(value *json.RawMessage) *json.RawMessage {
	if value == nil {
		return nil
	}
	copyValue := append(json.RawMessage(nil), (*value)...)
	return &copyValue
}
