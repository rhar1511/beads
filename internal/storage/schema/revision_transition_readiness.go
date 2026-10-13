package schema

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

const (
	RevisionTransitionMainMigration    = 69
	RevisionTransitionIgnoredMigration = 28
	RevisionTransitionPatchMigration   = 73
)

// RevisionTransitionReadiness is a startup proof that both migration planes,
// the receipt table, and the aggregate revision backfills are ready. Its zero
// value is deliberately not ready; callers cannot manufacture a positive
// result with a boolean config flag.
type RevisionTransitionReadiness struct {
	mainVersion    int
	ignoredVersion int
	verified       bool
	patchVerified  bool
}

// Ready reports whether ProbeRevisionTransitionReadiness verified every
// prerequisite for the transition/receipt API.
func (r RevisionTransitionReadiness) Ready() bool {
	return r.verified && r.mainVersion >= RevisionTransitionMainMigration && r.ignoredVersion >= RevisionTransitionIgnoredMigration
}

// PatchReady reports whether the additive atomic-patch receipt columns were
// verified. It deliberately remains false for databases that are ready for
// the compatible one-key transition API but have not applied migration 0073.
func (r RevisionTransitionReadiness) PatchReady() bool {
	return r.Ready() && r.patchVerified && r.mainVersion >= RevisionTransitionPatchMigration
}

// ProbeRevisionTransitionReadiness verifies the exact schema/data prerequisites
// used by revision-fenced transitions. It performs reads only. An error means
// readiness is unknown and must be treated as false by the caller.
func ProbeRevisionTransitionReadiness(ctx context.Context, db DBConn) (RevisionTransitionReadiness, error) {
	if db == nil {
		return RevisionTransitionReadiness{}, errors.New("schema: nil connection probing revision transition readiness")
	}
	mainVersion, err := CurrentVersion(ctx, db)
	if err != nil {
		return RevisionTransitionReadiness{}, fmt.Errorf("read main schema version: %w", err)
	}
	ignoredVersion, err := ignoredSource.currentVersion(ctx, db)
	if err != nil {
		return RevisionTransitionReadiness{}, fmt.Errorf("read ignored schema version: %w", err)
	}
	if mainVersion < RevisionTransitionMainMigration || ignoredVersion < RevisionTransitionIgnoredMigration {
		return RevisionTransitionReadiness{mainVersion: mainVersion, ignoredVersion: ignoredVersion}, nil
	}

	// The version cursors are not enough by themselves: the 0068/0028
	// backfills must have left no legacy zero token, and the table created by
	// 0069 must actually be queryable.
	for _, table := range []string{"issues", "wisps"} {
		var found int
		query := fmt.Sprintf("SELECT 1 FROM %s WHERE row_lock = 0 LIMIT 1", table)
		if err := db.QueryRowContext(ctx, query).Scan(&found); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return RevisionTransitionReadiness{}, fmt.Errorf("verify nonzero %s aggregate revisions: %w", table, err)
		} else if err == nil {
			return RevisionTransitionReadiness{mainVersion: mainVersion, ignoredVersion: ignoredVersion}, nil
		}
	}
	rows, err := db.QueryContext(ctx, "SELECT receipt_id FROM issue_transition_receipts LIMIT 0")
	if err != nil {
		return RevisionTransitionReadiness{}, fmt.Errorf("verify issue transition receipt table: %w", err)
	}
	if err := rows.Close(); err != nil {
		return RevisionTransitionReadiness{}, fmt.Errorf("close issue transition receipt probe: %w", err)
	}
	readiness := RevisionTransitionReadiness{mainVersion: mainVersion, ignoredVersion: ignoredVersion, verified: true}
	if mainVersion >= RevisionTransitionPatchMigration {
		patchRows, err := db.QueryContext(ctx, "SELECT request_json, receipt_json FROM issue_transition_receipts LIMIT 0")
		if err != nil {
			return RevisionTransitionReadiness{}, fmt.Errorf("verify atomic issue patch receipt columns: %w", err)
		}
		if err := patchRows.Close(); err != nil {
			return RevisionTransitionReadiness{}, fmt.Errorf("close atomic issue patch receipt probe: %w", err)
		}
		readiness.patchVerified = true
	}
	return readiness, nil
}
