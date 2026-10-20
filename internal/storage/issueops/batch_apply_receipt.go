package issueops

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/gob"
	"errors"
	"fmt"

	"github.com/rhar1511/beads/internal/storage"
	"github.com/rhar1511/beads/internal/storage/dberrors"
	publicops "github.com/rhar1511/beads/issueops"
)

const batchApplyReceiptResultVersion = 1

// ErrBatchApplyReceiptRace tells the role wrapper to reconcile the winning
// receipt in a fresh transaction. The caller must never rerun the batch on
// this path.
var ErrBatchApplyReceiptRace = errors.New("batch apply receipt identity raced")

// PreflightBatchApplyReceiptInTx returns the immutable result for a matching
// receipt or a stable conflict when the ID belongs to another request.
func PreflightBatchApplyReceiptInTx(ctx context.Context, tx DBTX, receiptID, requestDigest string) (publicops.ApplyBatchResult, bool, error) {
	receipt, err := getBatchApplyReceiptInTx(ctx, tx, receiptID)
	if errors.Is(err, sql.ErrNoRows) {
		return publicops.ApplyBatchResult{}, false, nil
	}
	if err != nil {
		return publicops.ApplyBatchResult{}, false, err
	}
	if receipt.RequestDigest != requestDigest {
		return publicops.ApplyBatchResult{}, false, fmt.Errorf("%w: receipt %s is bound to another request", publicops.ErrBatchApplyReceiptConflict, receiptID)
	}
	result := receipt.Result
	result.Replayed = true
	return result, true, nil
}

// GetBatchApplyReceiptInTx reads and decodes one receipt by exact caller ID.
func GetBatchApplyReceiptInTx(ctx context.Context, tx DBTX, receiptID string) (*publicops.BatchApplyReceipt, error) {
	if err := storage.ValidateBatchApplyReceiptID(receiptID); err != nil {
		return nil, err
	}
	receipt, err := getBatchApplyReceiptInTx(ctx, tx, receiptID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: batch apply receipt %s", storage.ErrNotFound, receiptID)
	}
	return receipt, err
}

func getBatchApplyReceiptInTx(ctx context.Context, tx DBTX, receiptID string) (*publicops.BatchApplyReceipt, error) {
	var receipt publicops.BatchApplyReceipt
	var version int
	var resultBlob []byte
	err := tx.QueryRowContext(ctx, `SELECT receipt_id, request_digest, result_version, result_blob
		FROM issue_batch_apply_receipts WHERE receipt_id = ?`, receiptID).
		Scan(&receipt.ReceiptID, &receipt.RequestDigest, &version, &resultBlob)
	if err != nil {
		return nil, err
	}
	if receipt.ReceiptID != receiptID || len(receipt.RequestDigest) != 64 {
		return nil, fmt.Errorf("batch apply receipt %s has invalid identity or request digest", receiptID)
	}
	if version != batchApplyReceiptResultVersion {
		return nil, fmt.Errorf("batch apply receipt %s has unsupported result version %d", receiptID, version)
	}
	if len(resultBlob) == 0 {
		return nil, fmt.Errorf("batch apply receipt %s has an empty result", receiptID)
	}
	if err := gob.NewDecoder(bytes.NewReader(resultBlob)).Decode(&receipt.Result); err != nil {
		return nil, fmt.Errorf("decode batch apply receipt %s result: %w", receiptID, err)
	}
	if receipt.Result.Replayed || receipt.Result.Keys == nil || receipt.Result.Items == nil {
		return nil, fmt.Errorf("batch apply receipt %s has an invalid stored result", receiptID)
	}
	return &receipt, nil
}

// StoreBatchApplyReceiptInTx writes the full original result in the same
// transaction as the batch mutations. Gob preserves Go-only snapshot members
// such as RowVersion and internal issue routing fields that JSON omits.
func StoreBatchApplyReceiptInTx(ctx context.Context, tx DBTX, plan storage.ApplyBatchPlan, result publicops.ApplyBatchResult) (publicops.ApplyBatchResult, error) {
	if plan.ReceiptID == "" {
		return publicops.ApplyBatchResult{}, fmt.Errorf("store batch apply receipt: receipt ID is required")
	}
	if err := storage.ValidateBatchApplyReceiptID(plan.ReceiptID); err != nil {
		return publicops.ApplyBatchResult{}, err
	}
	if plan.RequestDigest == "" {
		return publicops.ApplyBatchResult{}, fmt.Errorf("store batch apply receipt %s: request digest is required", plan.ReceiptID)
	}
	result.Replayed = false
	var encoded bytes.Buffer
	if err := gob.NewEncoder(&encoded).Encode(result); err != nil {
		return publicops.ApplyBatchResult{}, fmt.Errorf("encode batch apply receipt %s result: %w", plan.ReceiptID, err)
	}
	// Return the same canonical value a replay will decode. Gob deliberately
	// collapses empty slices to nil; decoding the just-written representation
	// keeps the first response and every replay structurally identical.
	var canonical publicops.ApplyBatchResult
	if err := gob.NewDecoder(bytes.NewReader(encoded.Bytes())).Decode(&canonical); err != nil {
		return publicops.ApplyBatchResult{}, fmt.Errorf("canonicalize batch apply receipt %s result: %w", plan.ReceiptID, err)
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO issue_batch_apply_receipts
		(receipt_id, request_digest, result_version, result_blob) VALUES (?, ?, ?, ?)`,
		plan.ReceiptID, plan.RequestDigest, batchApplyReceiptResultVersion, encoded.Bytes())
	if err != nil {
		if dberrors.IsDuplicateKey(err) {
			return publicops.ApplyBatchResult{}, fmt.Errorf("insert batch apply receipt %s: %w", plan.ReceiptID, ErrBatchApplyReceiptRace)
		}
		return publicops.ApplyBatchResult{}, fmt.Errorf("insert batch apply receipt %s: %w", plan.ReceiptID, err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return publicops.ApplyBatchResult{}, fmt.Errorf("insert batch apply receipt %s: rows affected: %w", plan.ReceiptID, err)
	}
	if inserted != 1 {
		return publicops.ApplyBatchResult{}, fmt.Errorf("insert batch apply receipt %s: inserted %d rows, want 1", plan.ReceiptID, inserted)
	}
	return canonical, nil
}
