package versioncontrolops

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/rhar1511/beads/internal/storage"
	"github.com/rhar1511/beads/internal/storage/issueops"
	"github.com/rhar1511/beads/internal/types"
)

// validTablePattern matches valid SQL table names (letters, digits, underscores).
var validTablePattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// Status returns the current Dolt working set status (staged and unstaged changes).
func Status(ctx context.Context, db DBConn) (*storage.Status, error) {
	rows, err := db.QueryContext(ctx, "SELECT table_name, staged, status FROM dolt_status")
	if err != nil {
		return nil, fmt.Errorf("get status: %w", err)
	}
	defer rows.Close()

	status := &storage.Status{
		Staged:   make([]storage.StatusEntry, 0),
		Unstaged: make([]storage.StatusEntry, 0),
	}

	for rows.Next() {
		var tableName string
		var staged bool
		var statusStr string
		if err := rows.Scan(&tableName, &staged, &statusStr); err != nil {
			return nil, fmt.Errorf("scan status: %w", err)
		}
		entry := storage.StatusEntry{Table: tableName, Status: statusStr}
		if staged {
			status.Staged = append(status.Staged, entry)
		} else {
			status.Unstaged = append(status.Unstaged, entry)
		}
	}
	return status, rows.Err()
}

// Log returns recent commit history up to limit entries.
// If limit is 0 or negative, all entries are returned.
func Log(ctx context.Context, db DBConn, limit int) ([]storage.CommitInfo, error) {
	var query string
	var args []interface{}
	if limit > 0 {
		query = "SELECT commit_hash, committer, email, date, message FROM dolt_log ORDER BY date DESC LIMIT ?"
		args = []interface{}{limit}
	} else {
		query = "SELECT commit_hash, committer, email, date, message FROM dolt_log ORDER BY date DESC"
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("get log: %w", err)
	}
	defer rows.Close()

	var commits []storage.CommitInfo
	for rows.Next() {
		var c storage.CommitInfo
		if err := rows.Scan(&c.Hash, &c.Author, &c.Email, &c.Date, &c.Message); err != nil {
			return nil, fmt.Errorf("scan commit: %w", err)
		}
		commits = append(commits, c)
	}
	return commits, rows.Err()
}

// CommitExists checks whether a commit hash (or prefix) exists in dolt_log.
// Returns false for empty strings or malformed input.
func CommitExists(ctx context.Context, db DBConn, commitHash string) (bool, error) {
	if commitHash == "" {
		return false, nil
	}
	if err := issueops.ValidateRef(commitHash); err != nil {
		return false, nil
	}

	var count int
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM dolt_log
		WHERE commit_hash = ? OR commit_hash LIKE ?
	`, commitHash, commitHash+"%").Scan(&count)
	if err != nil {
		return false, fmt.Errorf("check commit existence: %w", err)
	}
	return count > 0, nil
}

// Merge merges the named branch into the current branch. The author string
// should be formatted as "Name <email>". Returns any merge conflicts.
//
// This runs as a bare DOLT_MERGE under autocommit, so a real conflict makes
// Dolt reject the implicit transaction (Error 1105: "@autocommit must be
// disabled so that merge conflicts can be resolved ...") before dolt_conflicts
// can even be inspected — conflicts = error here, same as plain `dolt merge`
// with no further flags. Callers that want the flag Dolt's error names —
// resolve-then-commit on conflict — must use MergeWithStrategy instead, which
// runs the merge on a pinned session with the conflict-tolerant flags set
// (#4992).
func Merge(ctx context.Context, db DBConn, branch, author string) ([]storage.Conflict, error) {
	_, err := db.ExecContext(ctx, "CALL DOLT_MERGE('--author', ?, ?)", author, branch)
	if err != nil {
		// Check if the error is due to conflicts.
		conflicts, conflictErr := GetConflicts(ctx, db)
		if conflictErr == nil && len(conflicts) > 0 {
			return conflicts, nil
		}
		if isMergeConflictAutocommitError(err) {
			return nil, fmt.Errorf("merge branch %s: %w (resolve with: bd vc merge %s --strategy ours|theirs)", branch, err, branch)
		}
		return nil, fmt.Errorf("merge branch %s: %w", branch, err)
	}
	return nil, nil
}

// isMergeConflictAutocommitError reports whether err is Dolt's autocommit
// rejection of a conflicted merge (Error 1105, "@autocommit must be disabled
// so that merge conflicts can be resolved ..."). It is the shape Merge
// produces for every real conflict, since it runs under autocommit with
// neither dolt_allow_commit_conflicts nor a pinned session (#4992) — matched
// on message because the embedded engine and the MySQL driver report it as
// different error types.
func isMergeConflictAutocommitError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "merge conflict") && strings.Contains(msg, "autocommit")
}

// GetConflicts returns any merge conflicts in the current Dolt state.
func GetConflicts(ctx context.Context, db DBConn) ([]storage.Conflict, error) {
	rows, err := db.QueryContext(ctx, "SELECT `table`, num_conflicts FROM dolt_conflicts")
	if err != nil {
		return nil, fmt.Errorf("get conflicts: %w", err)
	}
	defer rows.Close()

	var conflicts []storage.Conflict
	for rows.Next() {
		var tableName string
		var numConflicts int
		if err := rows.Scan(&tableName, &numConflicts); err != nil {
			return nil, fmt.Errorf("scan conflict: %w", err)
		}
		conflicts = append(conflicts, storage.Conflict{
			Field: tableName,
			Count: numConflicts,
		})
	}
	return conflicts, rows.Err()
}

// ResolveConflicts resolves conflicts for a table using the given strategy
// ("ours" or "theirs").
func ResolveConflicts(ctx context.Context, db DBConn, table, strategy string) error {
	if err := validateTableName(table); err != nil {
		return fmt.Errorf("invalid table name: %w", err)
	}
	var query string
	switch strategy {
	case "ours":
		query = fmt.Sprintf("CALL DOLT_CONFLICTS_RESOLVE('--ours', '%s')", table)
	case "theirs":
		query = fmt.Sprintf("CALL DOLT_CONFLICTS_RESOLVE('--theirs', '%s')", table)
	default:
		return fmt.Errorf("unknown conflict resolution strategy: %s", strategy)
	}
	var dependencyFence dependencyConflictFence
	if table == "dependencies" {
		var err error
		dependencyFence, err = prepareDependencyConflictFence(ctx, db)
		if err != nil {
			return fmt.Errorf("prepare dependency conflict revision fence: %w", err)
		}
	}

	if _, err := db.ExecContext(ctx, query); err != nil {
		return fmt.Errorf("resolve conflicts: %w", err)
	}
	if table == "dependencies" {
		if err := finishDependencyConflictFence(ctx, db, dependencyFence); err != nil {
			return err
		}
	}
	return nil
}

type dependencyConflictEdge struct {
	source string
	target string
	kind   types.DependencyType
}

type dependencyConflictFence struct {
	sources []string
	edges   []dependencyConflictEdge
}

// prepareDependencyConflictFence gathers both sides of every dependency
// conflict before Dolt resolves them. A strategy may replace or remove any
// edge, so each surviving permanent source gets a new revision and the
// affected blocked-state closure is recomputed after resolution.
func prepareDependencyConflictFence(ctx context.Context, db DBConn) (dependencyConflictFence, error) {
	var fence dependencyConflictFence
	rows, err := db.QueryContext(ctx, `
		SELECT our_issue_id, their_issue_id,
		       our_depends_on_issue_id, their_depends_on_issue_id,
		       our_depends_on_wisp_id, their_depends_on_wisp_id,
		       our_depends_on_external, their_depends_on_external,
		       our_type, their_type
		FROM dolt_conflicts_dependencies`)
	if err != nil {
		return fence, fmt.Errorf("query dependency conflict rows: %w", err)
	}
	var edges []dependencyConflictEdge
	sourceSet := make(map[string]struct{})
	for rows.Next() {
		var sourceOurs, sourceTheirs sql.NullString
		var issueOurs, issueTheirs sql.NullString
		var wispOurs, wispTheirs sql.NullString
		var externalOurs, externalTheirs sql.NullString
		var typeOurs, typeTheirs sql.NullString
		if err := rows.Scan(&sourceOurs, &sourceTheirs, &issueOurs, &issueTheirs,
			&wispOurs, &wispTheirs, &externalOurs, &externalTheirs, &typeOurs, &typeTheirs); err != nil {
			_ = rows.Close()
			return fence, fmt.Errorf("scan dependency conflict row: %w", err)
		}
		for _, side := range []struct {
			source   sql.NullString
			issue    sql.NullString
			wisp     sql.NullString
			external sql.NullString
			kind     sql.NullString
		}{{sourceOurs, issueOurs, wispOurs, externalOurs, typeOurs}, {sourceTheirs, issueTheirs, wispTheirs, externalTheirs, typeTheirs}} {
			if !side.source.Valid || side.source.String == "" {
				continue
			}
			sourceSet[side.source.String] = struct{}{}
			if !side.kind.Valid || side.kind.String == "" {
				continue
			}
			target := ""
			switch {
			case side.issue.Valid:
				target = side.issue.String
			case side.wisp.Valid:
				target = side.wisp.String
			case side.external.Valid:
				target = side.external.String
			}
			edges = append(edges, dependencyConflictEdge{
				source: side.source.String,
				target: target,
				kind:   types.DependencyType(side.kind.String),
			})
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fence, fmt.Errorf("iterate dependency conflict rows: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fence, fmt.Errorf("close dependency conflict rows: %w", err)
	}

	if len(sourceSet) == 0 {
		return fence, nil
	}
	// Restrict fences to source rows that currently exist. A side can describe
	// an edge whose source issue was deleted by another merge conflict.
	sources := make([]string, 0, len(sourceSet))
	for source := range sourceSet {
		var exists int
		err := db.QueryRowContext(ctx, "SELECT 1 FROM issues WHERE id = ?", source).Scan(&exists)
		if err == nil {
			sources = append(sources, source)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return fence, fmt.Errorf("check dependency conflict source %s: %w", source, err)
		}
	}
	sort.Strings(sources)
	fence.sources = sources
	fence.edges = edges
	return fence, nil
}

func finishDependencyConflictFence(ctx context.Context, db DBConn, fence dependencyConflictFence) error {
	for _, source := range fence.sources {
		if err := issueops.BumpIssueRevisionInTx(ctx, db, source, false); err != nil {
			return fmt.Errorf("bump dependency conflict source %s: %w", source, err)
		}
	}
	issueSet := make(map[string]struct{})
	wispSet := make(map[string]struct{})
	for _, edge := range fence.edges {
		if edge.target == "" || !containsSorted(fence.sources, edge.source) {
			continue
		}
		issues, wisps, err := issueops.AffectedByDepChangeInTx(ctx, db, edge.source, edge.target, edge.kind)
		if err != nil {
			return fmt.Errorf("find affected rows for dependency conflict %s -> %s: %w", edge.source, edge.target, err)
		}
		for _, id := range issues {
			issueSet[id] = struct{}{}
		}
		for _, id := range wisps {
			wispSet[id] = struct{}{}
		}
	}
	recomputed, err := issueops.RecomputeIsBlockedInTxWithResult(ctx, db, sortedStringSet(issueSet), sortedStringSet(wispSet))
	if err != nil {
		return fmt.Errorf("recompute blocked state after dependency conflict resolution: %w", err)
	}
	if len(fence.sources) > 0 || recomputed.IssueRowsChanged {
		if _, err := db.ExecContext(ctx, "CALL DOLT_ADD('issues')"); err != nil {
			return fmt.Errorf("stage dependency source revisions after conflict resolution: %w", err)
		}
	}
	return nil
}

func sortedStringSet(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func containsSorted(values []string, want string) bool {
	i := sort.SearchStrings(values, want)
	return i < len(values) && values[i] == want
}

func validateTableName(table string) error {
	if table == "" {
		return fmt.Errorf("table name cannot be empty")
	}
	if len(table) > 64 {
		return fmt.Errorf("table name too long")
	}
	if !validTablePattern.MatchString(table) {
		return fmt.Errorf("invalid table name: %s", table)
	}
	return nil
}
