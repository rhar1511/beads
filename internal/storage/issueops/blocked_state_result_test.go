package issueops

import (
	"context"
	"database/sql/driver"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func newBlockedStateResultMock(t *testing.T) (sqlmock.Sqlmock, DBTX) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
		_ = db.Close()
	})
	return mock, db
}

func expectBlockedStatePass(mock sqlmock.Sqlmock, table, alias string, mark, unmark driver.Result) {
	mock.ExpectExec(regexp.QuoteMeta("UPDATE " + table + " " + alias + " SET " + alias + ".is_blocked = 1")).
		WillReturnResult(mark)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE " + table + " " + alias + " SET " + alias + ".is_blocked = 0")).
		WillReturnResult(unmark)
}

func TestRecomputeIsBlockedInTxWithResult(t *testing.T) {
	t.Run("issue rows changed", func(t *testing.T) {
		mock, db := newBlockedStateResultMock(t)
		expectNoProtectedRecords(mock, "issue-1")
		expectBlockedStatePass(mock, "issues", "i", sqlmock.NewResult(0, 1), sqlmock.NewResult(0, 0))
		expectBlockedStatePass(mock, "issues", "i", sqlmock.NewResult(0, 0), sqlmock.NewResult(0, 0))

		result, err := RecomputeIsBlockedInTxWithResult(context.Background(), db, []string{"issue-1"}, nil)
		if err != nil {
			t.Fatalf("RecomputeIsBlockedInTxWithResult: %v", err)
		}
		if !result.IssueRowsChanged || result.WispRowsChanged {
			t.Fatalf("result = %+v, want issue change only", result)
		}
	})

	t.Run("wisp rows changed", func(t *testing.T) {
		mock, db := newBlockedStateResultMock(t)
		expectNoProtectedRecords(mock, "wisp-1")
		expectBlockedStatePass(mock, "wisps", "w", sqlmock.NewResult(0, 0), sqlmock.NewResult(0, 1))
		expectBlockedStatePass(mock, "wisps", "w", sqlmock.NewResult(0, 0), sqlmock.NewResult(0, 0))

		result, err := RecomputeIsBlockedInTxWithResult(context.Background(), db, nil, []string{"wisp-1"})
		if err != nil {
			t.Fatalf("RecomputeIsBlockedInTxWithResult: %v", err)
		}
		if result.IssueRowsChanged || !result.WispRowsChanged {
			t.Fatalf("result = %+v, want wisp change only", result)
		}
	})

	t.Run("no rows changed", func(t *testing.T) {
		mock, db := newBlockedStateResultMock(t)
		expectNoProtectedRecords(mock, "issue-1", "wisp-1")
		expectBlockedStatePass(mock, "issues", "i", sqlmock.NewResult(0, 0), sqlmock.NewResult(0, 0))
		expectBlockedStatePass(mock, "wisps", "w", sqlmock.NewResult(0, 0), sqlmock.NewResult(0, 0))

		result, err := RecomputeIsBlockedInTxWithResult(
			context.Background(), db, []string{"issue-1"}, []string{"wisp-1"},
		)
		if err != nil {
			t.Fatalf("RecomputeIsBlockedInTxWithResult: %v", err)
		}
		if result.IssueRowsChanged || result.WispRowsChanged {
			t.Fatalf("result = %+v, want no changes", result)
		}
	})
}

func TestRunMarkUnmarkBatchedInTxPropagatesRowsAffectedErrors(t *testing.T) {
	sentinel := errors.New("rows affected unavailable")
	for _, phase := range []string{"mark", "unmark"} {
		t.Run(phase, func(t *testing.T) {
			mock, db := newBlockedStateResultMock(t)
			if phase == "mark" {
				mock.ExpectExec(regexp.QuoteMeta("UPDATE issues i SET i.is_blocked = 1")).
					WillReturnResult(sqlmock.NewErrorResult(sentinel))
			} else {
				mock.ExpectExec(regexp.QuoteMeta("UPDATE issues i SET i.is_blocked = 1")).
					WillReturnResult(sqlmock.NewResult(0, 0))
				mock.ExpectExec(regexp.QuoteMeta("UPDATE issues i SET i.is_blocked = 0")).
					WillReturnResult(sqlmock.NewErrorResult(sentinel))
			}

			_, err := runMarkUnmarkBatchedInTx(
				context.Background(), db, markBlockedTemplateForIssues(), unmarkBlockedTemplateForIssues(), []string{"issue-1"},
			)
			if !errors.Is(err, sentinel) {
				t.Fatalf("err = %v, want rows-affected error", err)
			}
		})
	}
}

// Batched blocked-state writes have one revision argument followed by six
// copies of the ID list. Repeating the token with each list breaks real SQL
// execution; dropping it leaves derived changes invisible to revision fences.
type distinctBlockedRevision struct{ seen map[int64]bool }

func (m distinctBlockedRevision) Match(value driver.Value) bool {
	token, ok := value.(int64)
	if !ok || token == 0 || m.seen[token] {
		return false
	}
	m.seen[token] = true
	return true
}

func TestBlockedBatchRevisionBindings(t *testing.T) {
	for _, plane := range []struct{ name, alias, mark, unmark string }{
		{"issues", "i", markBlockedTemplateForIssues(), unmarkBlockedTemplateForIssues()},
		{"wisps", "w", markBlockedTemplateForWisps(), unmarkBlockedTemplateForWisps()},
	} {
		for _, markOnly := range []bool{false, true} {
			name := plane.name + "/recompute"
			if markOnly {
				name = plane.name + "/mark"
			}
			t.Run(name, func(t *testing.T) {
				mock, db := newBlockedStateResultMock(t)
				revision := distinctBlockedRevision{seen: make(map[int64]bool)}
				args := []driver.Value{revision}
				for range batchTemplateOccurrences {
					args = append(args, "first", "second")
				}
				prefix := "UPDATE " + plane.name + " " + plane.alias + " SET " + plane.alias + ".is_blocked = "
				suffix := ", " + plane.alias + ".row_lock = ?, " + plane.alias + ".updated_at = " + plane.alias + ".updated_at"
				mock.ExpectExec(regexp.QuoteMeta(prefix + "1" + suffix)).WithArgs(args...).WillReturnResult(sqlmock.NewResult(0, 1))
				if !markOnly {
					mock.ExpectExec(regexp.QuoteMeta(prefix + "0" + suffix)).WithArgs(args...).WillReturnResult(sqlmock.NewResult(0, 1))
				}
				ids := []string{"first", "second"}
				var changed int64
				var err error
				want := int64(2)
				if markOnly {
					changed, err = runMarkBatchedInTx(context.Background(), db, plane.mark, ids)
					want = 1
				} else {
					changed, err = runMarkUnmarkBatchedInTx(context.Background(), db, plane.mark, plane.unmark, ids)
				}
				if err != nil || changed != want {
					t.Fatalf("changed=%d err=%v, want %d changed rows", changed, err, want)
				}
			})
		}
	}
}
