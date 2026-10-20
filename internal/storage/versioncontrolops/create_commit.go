package versioncontrolops

import (
	"context"
	"fmt"
	"sort"

	"github.com/rhar1511/beads/internal/storage/commitrows"
	"github.com/rhar1511/beads/internal/storage/issueops"
	"github.com/rhar1511/beads/internal/storage/schema"
)

// StageCreateAndCommit preserves create's actual issue-row scope. Other
// changed tables retain their existing whole-table staging semantics. SQL data
// must already be committed. On any error, staging progress remains available
// in the working set; callers must not replay the mutation or reset that work.
func StageCreateAndCommit(ctx context.Context, db DBConn, tables map[string]bool, images []commitrows.IssueRow, message, author string) error {
	if message == "" || len(tables) == 0 {
		return nil
	}
	if tables["issues"] && len(images) == 0 {
		return fmt.Errorf("%w: missing create issue-row scope", commitrows.ErrConflict)
	}
	var preexisting int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM dolt_status WHERE staged = TRUE").Scan(&preexisting); err != nil {
		return err
	}
	if preexisting != 0 {
		return fmt.Errorf("%w: preexisting staged work", commitrows.ErrConflict)
	}
	progress, err := commitrows.StageIssueRows(ctx, db, images)
	if err != nil {
		return fmt.Errorf("stage create rows (%d confirmed staged, %d absorbed): %w", len(progress.StagedIDs), len(progress.AbsorbedIDs), err)
	}
	ordered := make([]string, 0, len(tables))
	for table := range tables {
		if table != "issues" {
			ordered = append(ordered, table)
		}
	}
	sort.Strings(ordered)
	for _, table := range ordered {
		if err := schema.DrainCall(ctx, db, "CALL DOLT_ADD(?)", table); err != nil {
			return fmt.Errorf("dolt add %s: %w", table, err)
		}
	}
	rows, err := db.QueryContext(ctx, "SELECT table_name FROM dolt_status WHERE staged = TRUE")
	if err != nil {
		return err
	}
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			_ = rows.Close()
			return err
		}
		if !tables[table] {
			_ = rows.Close()
			return fmt.Errorf("%w: unknown staged table %s", commitrows.ErrConflict, table)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := commitrows.VerifyStaged(ctx, db, images, progress); err != nil {
		return err
	}
	staged, err := issueops.HasStagedChanges(ctx, db)
	if err != nil {
		return err
	}
	if !staged {
		return nil
	}
	if author == "" {
		err = schema.DrainCall(ctx, db, "CALL DOLT_COMMIT('-m', ?)", message)
	} else {
		err = schema.DrainCall(ctx, db, "CALL DOLT_COMMIT('-m', ?, '--author', ?)", message, author)
	}
	if err != nil && !issueops.IsNothingToCommitError(err) {
		return fmt.Errorf("dolt create commit: %w", err)
	}
	return nil
}
