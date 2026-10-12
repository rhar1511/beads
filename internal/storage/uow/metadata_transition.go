package uow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/domain"
	storeops "github.com/steveyegge/beads/internal/storage/issueops"
)

// MetadataTransitionRequest fences one issue metadata change with its row
// revision and assigns a durable, workspace-unique receipt ID. Optional JSON
// members distinguish an absent key from a present JSON null.
type MetadataTransitionRequest struct {
	ReceiptID       string           `json:"receipt_id"`
	IssueID         string           `json:"-"`
	Scope           string           `json:"scope"`
	Kind            string           `json:"kind"`
	Actor           string           `json:"actor"`
	ExpectedVersion string           `json:"expected_version"`
	Key             string           `json:"key"`
	Expected        *json.RawMessage `json:"expected,omitempty"`
	Value           *json.RawMessage `json:"value,omitempty"`
	Payload         json.RawMessage  `json:"payload,omitempty"`
	ProtectedPermit string           `json:"protected_permit,omitempty"`
}

type MetadataTransitionReceipt struct {
	ReceiptID       string           `json:"receipt_id"`
	IssueID         string           `json:"issue_id"`
	Scope           string           `json:"scope"`
	Kind            string           `json:"kind"`
	Actor           string           `json:"actor"`
	ExpectedVersion string           `json:"expected_version"`
	ToVersion       string           `json:"to_version"`
	Key             string           `json:"key"`
	Expected        *json.RawMessage `json:"expected,omitempty"`
	Value           *json.RawMessage `json:"value,omitempty"`
	Payload         json.RawMessage  `json:"payload"`
}

type MetadataTransitionResult struct {
	Applied  bool                       `json:"applied"`
	Replayed bool                       `json:"replayed"`
	Current  *json.RawMessage           `json:"current,omitempty"`
	Receipt  *MetadataTransitionReceipt `json:"receipt,omitempty"`
}

var ErrTransitionReceiptConflict = errors.New("metadata transition receipt belongs to another request")
var ErrTransitionPrecondition = errors.New("metadata transition precondition failed")

// TransitionMetadata writes the marker and its immutable receipt in the same
// UOW. A serialization loser re-runs the entire decision, beginning with the
// receipt lookup. This makes an exact retry after a lost HTTP response safe.
func TransitionMetadata(ctx context.Context, provider UnitOfWorkProvider, req MetadataTransitionRequest) (MetadataTransitionResult, error) {
	canonical, version, err := prepareMetadataTransition(req)
	if err != nil {
		return MetadataTransitionResult{}, err
	}
	return RunTxResult(ctx, provider, func(ctx context.Context, uw UnitOfWork) (MetadataTransitionResult, string, error) {
		previous, found, err := readMetadataTransitionReceipt(ctx, uw, canonical.ReceiptID)
		if err != nil {
			return MetadataTransitionResult{}, "", err
		}
		if found {
			if !sameMetadataTransition(previous, canonical) {
				return MetadataTransitionResult{}, "", ErrTransitionReceiptConflict
			}
			return MetadataTransitionResult{Applied: true, Replayed: true, Receipt: &previous}, "", nil
		}
		issue, err := uw.IssueUseCase().GetIssue(ctx, canonical.IssueID)
		if err != nil {
			return MetadataTransitionResult{}, "", err
		}
		if issue == nil {
			return MetadataTransitionResult{}, "", fmt.Errorf("%w: issue %s", storage.ErrNotFound, canonical.IssueID)
		}
		metadata := map[string]json.RawMessage{}
		if len(issue.Metadata) > 0 {
			if err := json.Unmarshal(issue.Metadata, &metadata); err != nil {
				return MetadataTransitionResult{}, "", fmt.Errorf("decode issue metadata: %w", err)
			}
		}
		var current *json.RawMessage
		if raw, ok := metadata[canonical.Key]; ok {
			value, err := storage.CanonicalMetadataValue(raw)
			if err != nil {
				return MetadataTransitionResult{}, "", err
			}
			current = &value
		}
		if issue.RowVersion != version || !sameOptionalJSON(current, canonical.Expected) {
			return MetadataTransitionResult{Current: current}, "", ErrTransitionPrecondition
		}
		fields := map[string]any{}
		if canonical.Value == nil {
			fields[storeops.OpUnsetMetadata] = []string{canonical.Key}
		} else {
			merge, err := json.Marshal(map[string]json.RawMessage{canonical.Key: *canonical.Value})
			if err != nil {
				return MetadataTransitionResult{}, "", err
			}
			fields[storeops.OpMergeMetadata] = json.RawMessage(merge)
		}
		updated, err := uw.IssueUseCase().ApplyUpdate(ctx, canonical.IssueID, domain.UpdateSpec{
			ExpectedVersion: &version, Fields: fields,
		}, canonical.Actor)
		if err != nil {
			return MetadataTransitionResult{}, "", err
		}
		if updated == nil || updated.RowVersion == version {
			return MetadataTransitionResult{}, "", errors.New("metadata transition did not advance issue revision")
		}
		receipt := MetadataTransitionReceipt{
			ReceiptID: canonical.ReceiptID, IssueID: canonical.IssueID, Scope: canonical.Scope,
			Kind: canonical.Kind, Actor: canonical.Actor, ExpectedVersion: canonical.ExpectedVersion,
			ToVersion: strconv.FormatInt(updated.RowVersion, 10), Key: canonical.Key,
			Expected: canonical.Expected, Value: canonical.Value, Payload: canonical.Payload,
		}
		if err := insertMetadataTransitionReceipt(ctx, uw, receipt); err != nil {
			return MetadataTransitionResult{}, "", err
		}
		return MetadataTransitionResult{Applied: true, Receipt: &receipt},
			fmt.Sprintf("bd: transition metadata %s.%s", canonical.IssueID, canonical.Key), nil
	})
}

func prepareMetadataTransition(req MetadataTransitionRequest) (MetadataTransitionRequest, int64, error) {
	if req.ProtectedPermit != "" {
		return req, 0, fmt.Errorf("protected metadata transitions are not supported")
	}
	for _, text := range []string{req.ReceiptID, req.IssueID, req.Scope, req.Kind, req.Actor, req.Key} {
		if text == "" || len(text) > 255 || !utf8.ValidString(text) || strings.ContainsFunc(text, func(r rune) bool {
			return r < 0x20 || r >= 0x7f && r <= 0x9f || r == '\u2028' || r == '\u2029'
		}) {
			return req, 0, fmt.Errorf("transition identifier must contain 1-255 bytes")
		}
	}
	if strings.TrimSpace(req.Actor) != req.Actor {
		return req, 0, fmt.Errorf("transition actor has surrounding whitespace")
	}
	version, err := strconv.ParseInt(req.ExpectedVersion, 10, 64)
	if err != nil || version == 0 || strconv.FormatInt(version, 10) != req.ExpectedVersion {
		return req, 0, fmt.Errorf("invalid expected revision")
	}
	if req.Expected, err = canonicalOptionalJSON(req.Expected); err != nil {
		return req, 0, err
	}
	if req.Value, err = canonicalOptionalJSON(req.Value); err != nil {
		return req, 0, err
	}
	if sameOptionalJSON(req.Expected, req.Value) {
		return req, 0, fmt.Errorf("metadata transition does not change the value")
	}
	if len(req.Payload) == 0 {
		req.Payload = json.RawMessage(`{}`)
	} else if req.Payload, err = storage.CanonicalMetadataValue(req.Payload); err != nil {
		return req, 0, err
	}
	return req, version, nil
}

func canonicalOptionalJSON(value *json.RawMessage) (*json.RawMessage, error) {
	if value == nil {
		return nil, nil
	}
	canonical, err := storage.CanonicalMetadataValue(*value)
	if err != nil {
		return nil, err
	}
	return &canonical, nil
}

func sameOptionalJSON(a, b *json.RawMessage) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return bytes.Equal(*a, *b)
}

func sameMetadataTransition(receipt MetadataTransitionReceipt, req MetadataTransitionRequest) bool {
	return receipt.ReceiptID == req.ReceiptID && receipt.IssueID == req.IssueID &&
		receipt.Scope == req.Scope && receipt.Kind == req.Kind && receipt.Actor == req.Actor &&
		receipt.ExpectedVersion == req.ExpectedVersion && receipt.Key == req.Key &&
		sameOptionalJSON(receipt.Expected, req.Expected) && sameOptionalJSON(receipt.Value, req.Value) &&
		bytes.Equal(receipt.Payload, req.Payload)
}

// MetadataTransitionReceiptByID looks up the committed outcome independently
// of the original issue request, allowing a caller to recover after an
// ambiguous POST result.
func MetadataTransitionReceiptByID(ctx context.Context, provider UnitOfWorkProvider, receiptID string) (MetadataTransitionReceipt, bool, error) {
	if receiptID == "" || len(receiptID) > 255 {
		return MetadataTransitionReceipt{}, false, nil
	}
	type lookup struct {
		receipt MetadataTransitionReceipt
		found   bool
	}
	result, err := RunTxRead(ctx, provider, func(ctx context.Context, uw UnitOfWork) (lookup, error) {
		receipt, found, err := readMetadataTransitionReceipt(ctx, uw, receiptID)
		return lookup{receipt, found}, err
	})
	return result.receipt, result.found, err
}

func readMetadataTransitionReceipt(ctx context.Context, uw UnitOfWork, id string) (MetadataTransitionReceipt, bool, error) {
	rows, err := uw.RawSQLUseCase().Query(ctx, `SELECT receipt_id, issue_id, scope, kind, actor,
		CAST(expected_version AS CHAR), CAST(to_version AS CHAR), metadata_key,
		expected_json, value_json, payload_json
		FROM issue_transition_receipts WHERE receipt_id = ?`, id)
	if err != nil {
		return MetadataTransitionReceipt{}, false, err
	}
	if len(rows.Rows) == 0 {
		return MetadataTransitionReceipt{}, false, nil
	}
	if len(rows.Rows) != 1 || len(rows.Rows[0]) != 11 {
		return MetadataTransitionReceipt{}, false, errors.New("malformed transition receipt row")
	}
	row := rows.Rows[0]
	value := func(i int) string { return fmt.Sprint(row[i]) }
	receipt := MetadataTransitionReceipt{
		ReceiptID: value(0), IssueID: value(1), Scope: value(2), Kind: value(3), Actor: value(4),
		ExpectedVersion: value(5), ToVersion: value(6), Key: value(7), Payload: json.RawMessage(value(10)),
	}
	if row[8] != nil {
		raw := json.RawMessage(value(8))
		receipt.Expected = &raw
	}
	if row[9] != nil {
		raw := json.RawMessage(value(9))
		receipt.Value = &raw
	}
	return receipt, true, nil
}

func insertMetadataTransitionReceipt(ctx context.Context, uw UnitOfWork, receipt MetadataTransitionReceipt) error {
	var expected, value any
	if receipt.Expected != nil {
		expected = string(*receipt.Expected)
	}
	if receipt.Value != nil {
		value = string(*receipt.Value)
	}
	_, err := uw.RawSQLUseCase().Exec(ctx, `INSERT INTO issue_transition_receipts
		(receipt_id, issue_id, scope, kind, actor, expected_version, to_version,
		 metadata_key, expected_json, value_json, payload_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		receipt.ReceiptID, receipt.IssueID, receipt.Scope, receipt.Kind, receipt.Actor,
		receipt.ExpectedVersion, receipt.ToVersion, receipt.Key, expected, value, string(receipt.Payload))
	return err
}
