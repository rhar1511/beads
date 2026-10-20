// Package commitrows stages explicitly captured durable issue-row postimages.
// It never commits, resets, or replays a data mutation. Call it only after the
// data transaction has committed, then retain its result even on error: some
// earlier rows may already have been staged.
package commitrows

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
)

var ErrConflict = errors.New("issue row staging conflict")
var columnName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

type Querier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type Conn interface {
	Querier
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// IssueRow is an immutable row image captured from a transaction's issues
// table. Its fields are private so a stage caller cannot omit a column guard.
type IssueRow struct {
	id          string
	columns     []string
	jsonColumns []bool
	values      []any
}

func (r IssueRow) ID() string { return r.id }

// Result describes actual staging progress, including progress before an
// error. AbsorbedIDs were already present exactly at HEAD and in WORKING.
// Neither list is a claim that this caller created a version commit.
type Result struct {
	StagedIDs   []string
	AbsorbedIDs []string
}

func freeze(value any) any {
	if b, ok := value.([]byte); ok {
		if b == nil {
			return []byte(nil)
		}
		copyValue := make([]byte, len(b))
		copy(copyValue, b)
		return copyValue
	}
	return value
}

func capture(ctx context.Context, db Querier, id, revision string) (IssueRow, error) {
	query := "SELECT * FROM issues WHERE id = ?"
	if revision == "HEAD" {
		query = "SELECT * FROM issues AS OF 'HEAD' WHERE id = ?"
	}
	rows, err := db.QueryContext(ctx, query, id)
	if err != nil {
		return IssueRow{}, fmt.Errorf("read issue postimage %s: %w", id, err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return IssueRow{}, err
	}
	columnTypes, err := rows.ColumnTypes()
	if err != nil {
		return IssueRow{}, err
	}
	if len(columnTypes) != len(columns) {
		return IssueRow{}, fmt.Errorf("%w: incomplete issue column types", ErrConflict)
	}
	jsonColumns := make([]bool, len(columns))
	for i, columnType := range columnTypes {
		jsonColumns[i] = strings.EqualFold(columnType.DatabaseTypeName(), "json")
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return IssueRow{}, err
		}
		return IssueRow{}, fmt.Errorf("read issue postimage %s: %w", id, sql.ErrNoRows)
	}
	values := make([]any, len(columns))
	dest := make([]any, len(columns))
	for i := range values {
		dest[i] = &values[i]
	}
	if err := rows.Scan(dest...); err != nil {
		return IssueRow{}, err
	}
	if rows.Next() {
		return IssueRow{}, fmt.Errorf("%w: multiple rows for issue %s", ErrConflict, id)
	}
	if err := rows.Err(); err != nil {
		return IssueRow{}, err
	}
	idSeen := false
	seen := make(map[string]bool, len(columns))
	for i, name := range columns {
		if !columnName.MatchString(name) || seen[name] {
			return IssueRow{}, fmt.Errorf("%w: invalid or duplicate issue column %q", ErrConflict, name)
		}
		seen[name] = true
		values[i] = freeze(values[i])
		if name == "id" {
			idSeen = true
			value := fmt.Sprint(values[i])
			if b, ok := values[i].([]byte); ok {
				value = string(b)
			}
			if value != id {
				return IssueRow{}, fmt.Errorf("%w: requested issue %s returned %s", ErrConflict, id, value)
			}
		}
	}
	if !idSeen {
		return IssueRow{}, fmt.Errorf("%w: missing issue id column", ErrConflict)
	}
	if err := rows.Close(); err != nil {
		return IssueRow{}, err
	}
	return IssueRow{id: id, columns: append([]string(nil), columns...), jsonColumns: jsonColumns, values: values}, nil
}

// CaptureIssueRows reads exact postimages on the data transaction. IDs must be
// the rows it actually wrote, not all request endpoints or closure candidates.
// Admission of preexisting dirty/staged work belongs before the data commit;
// capturing an image does not itself establish ownership of that row.
func CaptureIssueRows(ctx context.Context, db Querier, ids []string) ([]IssueRow, error) {
	unique := make(map[string]bool, len(ids))
	ordered := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			return nil, fmt.Errorf("%w: empty issue row id", ErrConflict)
		}
		if !unique[id] {
			unique[id] = true
			ordered = append(ordered, id)
		}
	}
	sort.Strings(ordered)
	out := make([]IssueRow, 0, len(ordered))
	for _, id := range ordered {
		image, err := capture(ctx, db, id, "")
		if err != nil {
			return nil, err
		}
		out = append(out, image)
	}
	return out, nil
}

func same(a, b IssueRow) bool {
	return a.id == b.id && reflect.DeepEqual(a.columns, b.columns) && reflect.DeepEqual(a.jsonColumns, b.jsonColumns) && reflect.DeepEqual(a.values, b.values)
}

// StageIssueRows conditionally stages each whole row only while every column
// matches its captured postimage. It refuses preexisting staged work. A
// postimage conflict preserves unknown changes and returns progress; callers
// must not commit or blindly retry that progress. It is not a lock spanning a
// later version commit, nor a claim of field-level or multi-row atomic staging.
func StageIssueRows(ctx context.Context, db Conn, images []IssueRow) (Result, error) {
	var result Result
	if len(images) == 0 {
		return result, nil
	}
	seen := make(map[string]bool, len(images))
	for _, image := range images {
		if image.id == "" || len(image.columns) == 0 || len(image.columns) != len(image.values) || len(image.jsonColumns) != len(image.columns) || seen[image.id] {
			return result, fmt.Errorf("%w: invalid or duplicate issue image", ErrConflict)
		}
		seen[image.id] = true
	}
	var staged int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM dolt_status WHERE staged = TRUE").Scan(&staged); err != nil {
		return result, fmt.Errorf("inspect staged work: %w", err)
	}
	if staged != 0 {
		return result, fmt.Errorf("%w: preexisting staged work", ErrConflict)
	}
	for _, image := range images {
		conditions := []string{"staged = FALSE"}
		args := make([]any, 0, len(image.columns))
		for i, name := range image.columns {
			parameter := "?"
			if image.jsonColumns[i] {
				// Drivers return JSON as text. Compare JSON with JSON rather
				// than treating the captured document as a JSON string value.
				parameter = "CAST(? AS JSON)"
			}
			conditions = append(conditions, "`to_"+name+"` <=> "+parameter)
			args = append(args, freeze(image.values[i]))
		}
		query := "UPDATE dolt_workspace_issues SET staged = TRUE WHERE " + strings.Join(conditions, " AND ")
		changed, err := db.ExecContext(ctx, query, args...)
		if err != nil {
			return result, fmt.Errorf("stage issue %s: %w", image.id, err)
		}
		n, err := changed.RowsAffected()
		if err != nil {
			return result, fmt.Errorf("stage issue %s result: %w", image.id, err)
		}
		if n == 1 {
			result.StagedIDs = append(result.StagedIDs, image.id)
			continue
		}
		if n != 0 {
			return result, fmt.Errorf("%w: issue %s staged %d rows", ErrConflict, image.id, n)
		}
		working, err := capture(ctx, db, image.id, "")
		if err != nil {
			return result, fmt.Errorf("verify issue %s after unmatched stage: %w", image.id, err)
		}
		head, err := capture(ctx, db, image.id, "HEAD")
		if err != nil {
			return result, fmt.Errorf("verify issue %s HEAD after unmatched stage: %w", image.id, err)
		}
		if !same(image, working) || !same(image, head) {
			return result, fmt.Errorf("%w: issue %s postimage no longer matches", ErrConflict, image.id)
		}
		result.AbsorbedIDs = append(result.AbsorbedIDs, image.id)
	}
	return result, nil
}
