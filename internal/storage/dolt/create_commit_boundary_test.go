package dolt

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"

	mysql "github.com/go-sql-driver/mysql"
	"github.com/rhar1511/beads/internal/storage"
	storageissueops "github.com/rhar1511/beads/internal/storage/issueops"
	"github.com/rhar1511/beads/internal/types"
	publicops "github.com/rhar1511/beads/issueops"
)

// Reuse the existing narrow sequence driver: these cases own ordering and
// replay/error disposition, not Dolt persistence or server parity.
func createCommitSequence(t *testing.T) (*seqRecorder, *seqConnector, *sql.DB) {
	t.Helper()
	rec := &seqRecorder{}
	connector := &seqConnector{rec: rec}
	staged := false
	connector.rows = func(query string) (driver.Rows, bool) {
		switch {
		case strings.Contains(query, "DOLT_ADD"):
			staged = true
		case strings.Contains(query, "FROM dolt_workspace_issues"):
			return &valRows{cols: []string{"count"}, vals: [][]driver.Value{{int64(0)}}}, true
		case strings.HasPrefix(query, "SELECT table_name FROM dolt_status"):
			rows := &valRows{cols: []string{"table_name"}}
			if staged {
				rows.vals = [][]driver.Value{{"metadata"}}
			}
			return rows, true
		case strings.Contains(query, "FROM dolt_status"):
			count := int64(0)
			if staged {
				count = 1
			}
			return &valRows{cols: []string{"count"}, vals: [][]driver.Value{{count}}}, true
		}
		return nil, false
	}
	db := sql.OpenDB(connector)
	db.SetMaxIdleConns(0)
	t.Cleanup(func() { _ = db.Close() })
	return rec, connector, db
}

func TestCreateCommitBoundaryKeepsSQLAndHistoryDispositions(t *testing.T) {
	for _, mode := range []string{"success", "stage refusal", "history response loss", "data response loss", "body refusal"} {
		t.Run(mode, func(t *testing.T) {
			rec, connector, db := createCommitSequence(t)
			cause := errors.New("injected " + mode)
			if mode == "data response loss" {
				connector.commitErr = func() error { return cause }
			}
			connector.queryErr = func(query string) error {
				if mode == "stage refusal" && strings.Contains(query, "DOLT_ADD") {
					return &mysql.MySQLError{Number: 1213, Message: "injected history serialization failure"}
				}
				if mode == "history response loss" && strings.Contains(query, "DOLT_COMMIT") {
					return cause
				}
				return nil
			}
			calls := 0
			s := &DoltStore{db: db}
			result, err := s.runCreateIssueOperationTx(t.Context(), func(tx *sql.Tx) (publicops.CreateResult, storageissueops.CreateCommitScope, error) {
				calls++
				if mode == "body refusal" {
					return publicops.CreateResult{}, storageissueops.CreateCommitScope{}, cause
				}
				if _, err := tx.ExecContext(t.Context(), "UPDATE metadata SET value = 'applied' WHERE `key` = 'create-test'"); err != nil {
					return publicops.CreateResult{}, storageissueops.CreateCommitScope{}, err
				}
				return publicops.CreateResult{Issue: &types.Issue{ID: "applied"}}, storageissueops.CreateCommitScope{Tables: storageissueops.ChangedTables{"metadata": true}}, nil
			})
			if calls != 1 {
				t.Fatalf("mutation callback replayed: %d", calls)
			}
			events, conns := rec.snapshotConns()
			dataCommit, add, history := -1, -1, -1
			adds, commits := 0, 0
			publicationConn := 0
			for i, event := range events {
				if event == "COMMIT" {
					dataCommit = i
				}
				if strings.Contains(event, "DOLT_ADD") {
					adds++
					add = i
				}
				if strings.Contains(event, "DOLT_COMMIT") {
					commits++
					history = i
				}
				if strings.Contains(event, "DOLT_ADD") || strings.Contains(event, "DOLT_COMMIT") {
					if publicationConn == 0 {
						publicationConn = conns[i]
					} else if publicationConn != conns[i] {
						t.Fatalf("publication changed connection: %v", events)
					}
				}
			}
			switch mode {
			case "data response loss":
				if !errors.Is(err, storage.ErrCommitIndeterminate) || !errors.Is(err, cause) {
					t.Fatalf("ambiguous data commit error = %v", err)
				}
				if adds != 0 || commits != 0 {
					t.Fatalf("history ran after ambiguous data commit: %v", events)
				}
			case "body refusal":
				if !errors.Is(err, cause) || dataCommit != -1 || adds != 0 || commits != 0 {
					t.Fatalf("refusal outcome err=%v events=%v", err, events)
				}
			default:
				if err != nil || result.Issue == nil || result.Issue.ID != "applied" {
					t.Fatalf("applied result lost: %#v, %v", result, err)
				}
				if dataCommit < 0 || add < dataCommit || adds != 1 {
					t.Fatalf("post-SQL stage order/retry violated: %v", events)
				}
				if mode == "stage refusal" {
					if commits != 0 {
						t.Fatalf("history committed after stage failure: %v", events)
					}
				} else if commits != 1 || history < add {
					t.Fatalf("history order/replay violated: %v", events)
				}
			}
		})
	}
}

func TestCreateCommitScopeDoesNotLeakFromRolledBackAttempt(t *testing.T) {
	rec, _, db := createCommitSequence(t)
	s := &DoltStore{db: db}
	calls := 0
	result, err := s.runCreateIssueOperationTx(t.Context(), func(*sql.Tx) (publicops.CreateResult, storageissueops.CreateCommitScope, error) {
		calls++
		if calls == 1 {
			return publicops.CreateResult{Issue: &types.Issue{ID: "rolled-back"}}, storageissueops.CreateCommitScope{Tables: storageissueops.ChangedTables{"issues": true}}, &mysql.MySQLError{Number: 1213, Message: "known rollback"}
		}
		return publicops.CreateResult{Issue: &types.Issue{ID: "accepted"}}, storageissueops.CreateCommitScope{}, nil
	})
	if err != nil || calls != 2 || result.Issue == nil || result.Issue.ID != "accepted" {
		t.Fatalf("retry result=%#v err=%v calls=%d", result, err, calls)
	}
	for _, event := range rec.snapshot() {
		if strings.Contains(event, "DOLT_ADD") || strings.Contains(event, "DOLT_COMMIT") {
			t.Fatalf("rolled-back scope leaked: %v", rec.snapshot())
		}
	}
}

func TestCreateCommitPostTxUsesDetachedBoundedContext(t *testing.T) {
	rec, _, db := createCommitSequence(t)
	s := &DoltStore{db: db}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.createCommitPostTx(ctx, storageissueops.CreateCommitScope{Tables: storageissueops.ChangedTables{"metadata": true}}); err != nil {
		t.Fatalf("caller cancellation skipped post-data history: %v", err)
	}
	sawCommit := false
	for _, event := range rec.snapshot() {
		if strings.Contains(event, "DOLT_COMMIT") {
			sawCommit = true
		}
	}
	if !sawCommit {
		t.Fatalf("detached history never attempted: %v", rec.snapshot())
	}
}
