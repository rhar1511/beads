package issueops

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/rhar1511/beads/internal/storage/commitrows"
	publicops "github.com/rhar1511/beads/issueops"
)

// CreateCommitScope is one data transaction's actual durable row writes.
// It never crosses a rolled-back attempt or authorizes replay of a mutation.
type CreateCommitScope struct {
	Tables    ChangedTables
	IssueRows []commitrows.IssueRow
	admission commitrows.Admission
}

// ExecuteCreateWithCommit keeps the public create result unchanged while
// carrying its actual issue-row postimages to the post-SQL-commit boundary.
// Existing batch callers keep ExecuteCreate's table-only staging contract.
func ExecuteCreateWithCommit(ctx context.Context, tx *sql.Tx, request publicops.CreateRequest) (publicops.CreateResult, CreateCommitScope, error) {
	var scope CreateCommitScope
	if err := ValidatePublicCreateRequest(CloneCreateRequest(request)); err != nil {
		return publicops.CreateResult{}, scope, err
	}
	result, tables, err := executeCreate(ctx, tx, request, &scope)
	if err != nil {
		return publicops.CreateResult{}, CreateCommitScope{}, err
	}
	scope.Tables = tables
	if ProtectedMutationPermitUseWritten(ctx) {
		scope.Tables.Add("protected_mutation_permit_uses")
	}
	if err := scope.admission.CheckCommit(scope.IssueRows, len(scope.Tables) > 0); err != nil {
		return publicops.CreateResult{}, CreateCommitScope{}, err
	}
	return result, scope, nil
}

// Only the create path collecting row scope pays these reads. Comparing row
// tokens around blocked recomputation identifies actual derived writes, not
// every possible closure endpoint. Legacy/import/batch entry points retain
// their existing query and staging contract.
func captureCreateIssueVersions(ctx context.Context, tx DBTX, ids []string) (map[string]string, error) {
	versions := make(map[string]string, len(ids))
	for start := 0; start < len(ids); start += queryBatchSize {
		end := start + queryBatchSize
		if end > len(ids) {
			end = len(ids)
		}
		placeholders, args := buildSQLInClause(ids[start:end])
		//nolint:gosec // The interpolated string contains only bound placeholders.
		rows, err := tx.QueryContext(ctx, fmt.Sprintf("SELECT id, row_lock FROM issues WHERE id IN (%s)", placeholders), args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, version string
			if err := rows.Scan(&id, &version); err != nil {
				_ = rows.Close()
				return nil, err
			}
			versions[id] = version
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return versions, nil
}
