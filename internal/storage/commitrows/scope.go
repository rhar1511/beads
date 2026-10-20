package commitrows

import (
	"context"
	"fmt"
	"strings"
)

// Admission remembers issue rows already dirty before a create. A later
// same-row write refuses before SQL commit instead of adopting that dirt.
// It is not a lock against other sessions.
type Admission struct {
	dirty  map[string]bool
	staged bool
}

func InspectAdmission(ctx context.Context, db Querier) (Admission, error) {
	admission := Admission{dirty: make(map[string]bool)}
	var staged int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM dolt_status WHERE staged = TRUE").Scan(&staged); err != nil {
		return admission, err
	}
	admission.staged = staged != 0
	rows, err := db.QueryContext(ctx, "SELECT COALESCE(to_id, from_id) FROM dolt_workspace_issues")
	if err != nil {
		return admission, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return admission, err
		}
		if id == "" {
			return admission, fmt.Errorf("%w: dirty issue without identity", ErrConflict)
		}
		admission.dirty[id] = true
	}
	if err := rows.Err(); err != nil {
		return admission, err
	}
	return admission, rows.Close()
}

func (a Admission) CheckCommit(images []IssueRow, needsCommit bool) error {
	if needsCommit && a.staged {
		return fmt.Errorf("%w: preexisting staged work", ErrConflict)
	}
	for _, image := range images {
		if a.dirty[image.id] {
			return fmt.Errorf("%w: issue %s was already dirty before create", ErrConflict, image.id)
		}
	}
	return nil
}

func imagePredicate(image IssueRow) (string, []any) {
	clauses := make([]string, 0, len(image.columns))
	args := make([]any, 0, len(image.columns))
	for i, name := range image.columns {
		parameter := "?"
		if image.jsonColumns[i] {
			parameter = "CAST(? AS JSON)"
		}
		clauses = append(clauses, "`to_"+name+"` <=> "+parameter)
		args = append(args, freeze(image.values[i]))
	}
	return strings.Join(clauses, " AND "), args
}

// VerifyStaged checks the issue-row set and captured images immediately before
// the caller's commit. Unknown staged rows refuse; no reset or retry occurs.
// Shared-branch raw writers can still race a later commit: this is a scope
// check, not an atomic compare-and-commit primitive.
func VerifyStaged(ctx context.Context, db Querier, images []IssueRow, progress Result) error {
	byID := make(map[string]IssueRow, len(images))
	for _, image := range images {
		byID[image.id] = image
	}
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM dolt_workspace_issues WHERE staged = TRUE").Scan(&count); err != nil {
		return err
	}
	if count != len(progress.StagedIDs) {
		return fmt.Errorf("%w: staged issue scope changed", ErrConflict)
	}
	for _, id := range progress.StagedIDs {
		image, ok := byID[id]
		if !ok {
			return fmt.Errorf("%w: unknown staged issue %s", ErrConflict, id)
		}
		predicate, args := imagePredicate(image)
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM dolt_workspace_issues WHERE staged = TRUE AND "+predicate, args...).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("%w: staged issue %s postimage changed", ErrConflict, id)
		}
	}
	for _, id := range progress.AbsorbedIDs {
		image, ok := byID[id]
		if !ok {
			return fmt.Errorf("%w: unknown absorbed issue %s", ErrConflict, id)
		}
		working, err := capture(ctx, db, id, "")
		if err != nil {
			return err
		}
		head, err := capture(ctx, db, id, "HEAD")
		if err != nil {
			return err
		}
		if !same(image, working) || !same(image, head) {
			return fmt.Errorf("%w: absorbed issue %s changed", ErrConflict, id)
		}
	}
	return nil
}
