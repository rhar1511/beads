package schema

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const ProtectedMutationMainMigration = 70

// ProtectedMutationReadiness is a startup proof that the explicit record
// classification and one-time permit-use tables are present. Its zero value
// is deliberately not ready; callers cannot manufacture readiness with a
// boolean configuration flag.
type ProtectedMutationReadiness struct {
	mainVersion int
	verified    bool
}

// Ready reports whether ProbeProtectedMutationReadiness verified both tables,
// their enforcement-critical column shapes, and their primary keys.
func (r ProtectedMutationReadiness) Ready() bool {
	return r.verified && r.mainVersion >= ProtectedMutationMainMigration
}

// ProbeProtectedMutationReadiness verifies the schema prerequisites for
// protected mutation enforcement. It performs reads only. An error means
// readiness is unknown and callers must fail closed.
func ProbeProtectedMutationReadiness(ctx context.Context, db DBConn) (ProtectedMutationReadiness, error) {
	if db == nil {
		return ProtectedMutationReadiness{}, fmt.Errorf("schema: nil connection probing protected mutation readiness")
	}
	mainVersion, err := CurrentVersion(ctx, db)
	if err != nil {
		return ProtectedMutationReadiness{}, fmt.Errorf("read main schema version: %w", err)
	}
	if mainVersion < ProtectedMutationMainMigration {
		return ProtectedMutationReadiness{mainVersion: mainVersion}, nil
	}

	for _, query := range []string{
		"SELECT issue_id, protection_class FROM protected_issue_records LIMIT 0",
		"SELECT audience, replay_id FROM protected_mutation_permit_uses LIMIT 0",
	} {
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			return ProtectedMutationReadiness{}, fmt.Errorf("verify protected mutation table: %w", err)
		}
		if err := rows.Close(); err != nil {
			return ProtectedMutationReadiness{}, fmt.Errorf("close protected mutation table probe: %w", err)
		}
	}
	for _, table := range []struct {
		name    string
		columns []string
	}{
		{name: "protected_issue_records", columns: []string{"issue_id"}},
		{name: "protected_mutation_permit_uses", columns: []string{"audience", "replay_id"}},
	} {
		ready, err := protectedMutationPrimaryKeyReady(ctx, db, table.name, table.columns)
		if err != nil {
			return ProtectedMutationReadiness{}, fmt.Errorf("verify protected mutation replay key: %w", err)
		}
		if !ready {
			return ProtectedMutationReadiness{mainVersion: mainVersion}, nil
		}
	}
	for _, column := range []struct {
		table     string
		name      string
		maxLength int64
	}{
		{table: "protected_issue_records", name: "issue_id", maxLength: 255},
		{table: "protected_issue_records", name: "protection_class", maxLength: 64},
		{table: "protected_mutation_permit_uses", name: "audience", maxLength: 255},
		{table: "protected_mutation_permit_uses", name: "replay_id", maxLength: 255},
	} {
		ready, err := protectedMutationColumnReady(ctx, db, column.table, column.name, column.maxLength)
		if err != nil {
			return ProtectedMutationReadiness{}, fmt.Errorf("verify protected mutation column %s.%s: %w", column.table, column.name, err)
		}
		if !ready {
			return ProtectedMutationReadiness{mainVersion: mainVersion}, nil
		}
	}
	return ProtectedMutationReadiness{mainVersion: mainVersion, verified: true}, nil
}

func protectedMutationColumnReady(ctx context.Context, db DBConn, table, column string, expectedLength int64) (bool, error) {
	var dataType string
	var maxLength sql.NullInt64
	var nullable string
	err := db.QueryRowContext(ctx, `SELECT data_type, character_maximum_length, is_nullable
		FROM information_schema.columns
		WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?`, table, column).
		Scan(&dataType, &maxLength, &nullable)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	ready := strings.EqualFold(dataType, "varchar") && maxLength.Valid &&
		maxLength.Int64 == expectedLength && strings.EqualFold(nullable, "NO")
	return ready, nil
}

func protectedMutationPrimaryKeyReady(ctx context.Context, db DBConn, table string, expected []string) (bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT column_name FROM information_schema.statistics
		WHERE table_schema = DATABASE() AND table_name = ? AND index_name = 'PRIMARY'
		ORDER BY seq_in_index`, table)
	if err != nil {
		return false, err
	}
	var columns []string
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			_ = rows.Close()
			return false, err
		}
		columns = append(columns, column)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return false, err
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	if len(columns) != len(expected) {
		return false, nil
	}
	for i := range expected {
		if columns[i] != expected[i] {
			return false, nil
		}
	}
	return true, nil
}
