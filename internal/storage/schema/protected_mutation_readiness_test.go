package schema

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestProtectedMutationReadinessRequiresMigrationAndBothTables(t *testing.T) {
	t.Run("migration absent", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		expectVersionCursor(mock, "schema_migrations", ProtectedMutationMainMigration-1)
		readiness, err := ProbeProtectedMutationReadiness(context.Background(), db)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if readiness.Ready() {
			t.Fatal("partial migration state reported ready")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("replay table unavailable", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		expectVersionCursor(mock, "schema_migrations", ProtectedMutationMainMigration)
		mock.ExpectQuery(regexp.QuoteMeta("SELECT issue_id, protection_class FROM protected_issue_records LIMIT 0")).
			WillReturnRows(sqlmock.NewRows([]string{"issue_id", "protection_class"}))
		missing := errors.New("table absent")
		mock.ExpectQuery(regexp.QuoteMeta("SELECT audience, replay_id FROM protected_mutation_permit_uses LIMIT 0")).
			WillReturnError(missing)
		readiness, err := ProbeProtectedMutationReadiness(context.Background(), db)
		if !errors.Is(err, missing) {
			t.Fatalf("probe error = %v, want missing replay table", err)
		}
		if readiness.Ready() {
			t.Fatal("missing replay table reported ready")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("both tables ready", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		expectVersionCursor(mock, "schema_migrations", ProtectedMutationMainMigration)
		mock.ExpectQuery(regexp.QuoteMeta("SELECT issue_id, protection_class FROM protected_issue_records LIMIT 0")).
			WillReturnRows(sqlmock.NewRows([]string{"issue_id", "protection_class"}))
		mock.ExpectQuery(regexp.QuoteMeta("SELECT audience, replay_id FROM protected_mutation_permit_uses LIMIT 0")).
			WillReturnRows(sqlmock.NewRows([]string{"audience", "replay_id"}))
		expectProtectedMutationPrimaryKeys(mock)
		expectProtectedMutationColumnShapes(mock)
		readiness, err := ProbeProtectedMutationReadiness(context.Background(), db)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if !readiness.Ready() {
			t.Fatal("both protected mutation tables were not reported ready")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("enforcement column shape is exact", func(t *testing.T) {
		for _, tc := range []struct {
			name      string
			index     int
			dataType  string
			maxLength any
			nullable  string
		}{
			{name: "nullable issue id", index: 0, dataType: "varchar", maxLength: int64(255), nullable: "YES"},
			{name: "wrong-sized issue id", index: 0, dataType: "varchar", maxLength: int64(254), nullable: "NO"},
			{name: "wrong-type issue id", index: 0, dataType: "text", maxLength: nil, nullable: "NO"},
			{name: "nullable protection class", index: 1, dataType: "varchar", maxLength: int64(64), nullable: "YES"},
			{name: "wrong-sized protection class", index: 1, dataType: "varchar", maxLength: int64(63), nullable: "NO"},
			{name: "wrong-type protection class", index: 1, dataType: "text", maxLength: nil, nullable: "NO"},
			{name: "nullable permit audience", index: 2, dataType: "varchar", maxLength: int64(255), nullable: "YES"},
			{name: "wrong-sized permit audience", index: 2, dataType: "varchar", maxLength: int64(254), nullable: "NO"},
			{name: "wrong-type permit audience", index: 2, dataType: "text", maxLength: nil, nullable: "NO"},
			{name: "nullable replay id", index: 3, dataType: "varchar", maxLength: int64(255), nullable: "YES"},
			{name: "wrong-sized replay id", index: 3, dataType: "varchar", maxLength: int64(254), nullable: "NO"},
			{name: "wrong-type replay id", index: 3, dataType: "text", maxLength: nil, nullable: "NO"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				db, mock, err := sqlmock.New()
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				expectVersionCursor(mock, "schema_migrations", ProtectedMutationMainMigration)
				expectProtectedMutationTableProbes(mock)
				expectProtectedMutationPrimaryKeys(mock)
				expectProtectedMutationColumnShapesUntil(mock, tc.index, tc.dataType, tc.maxLength, tc.nullable)
				readiness, err := ProbeProtectedMutationReadiness(context.Background(), db)
				if err != nil {
					t.Fatalf("probe: %v", err)
				}
				if readiness.Ready() {
					t.Fatal("malformed enforcement column reported ready")
				}
				if err := mock.ExpectationsWereMet(); err != nil {
					t.Fatal(err)
				}
			})
		}
	})

	t.Run("replay identity is not unique", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		expectVersionCursor(mock, "schema_migrations", ProtectedMutationMainMigration)
		mock.ExpectQuery(regexp.QuoteMeta("SELECT issue_id, protection_class FROM protected_issue_records LIMIT 0")).
			WillReturnRows(sqlmock.NewRows([]string{"issue_id", "protection_class"}))
		mock.ExpectQuery(regexp.QuoteMeta("SELECT audience, replay_id FROM protected_mutation_permit_uses LIMIT 0")).
			WillReturnRows(sqlmock.NewRows([]string{"audience", "replay_id"}))
		mock.ExpectQuery(regexp.QuoteMeta(`SELECT column_name FROM information_schema.statistics
		WHERE table_schema = DATABASE() AND table_name = ? AND index_name = 'PRIMARY'
		ORDER BY seq_in_index`)).WithArgs("protected_issue_records").
			WillReturnRows(sqlmock.NewRows([]string{"column_name"}).AddRow("issue_id"))
		mock.ExpectQuery(regexp.QuoteMeta(`SELECT column_name FROM information_schema.statistics
		WHERE table_schema = DATABASE() AND table_name = ? AND index_name = 'PRIMARY'
		ORDER BY seq_in_index`)).WithArgs("protected_mutation_permit_uses").
			WillReturnRows(sqlmock.NewRows([]string{"column_name"}).AddRow("audience"))
		readiness, err := ProbeProtectedMutationReadiness(context.Background(), db)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if readiness.Ready() {
			t.Fatal("replay table without the audience/replay identity key reported ready")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestZeroProtectedMutationReadinessIsUnknown(t *testing.T) {
	var readiness ProtectedMutationReadiness
	if readiness.Ready() {
		t.Fatal("zero-value readiness proof is ready")
	}
}

func expectProtectedMutationPrimaryKeys(mock sqlmock.Sqlmock) {
	query := regexp.QuoteMeta(`SELECT column_name FROM information_schema.statistics
		WHERE table_schema = DATABASE() AND table_name = ? AND index_name = 'PRIMARY'
		ORDER BY seq_in_index`)
	mock.ExpectQuery(query).WithArgs("protected_issue_records").
		WillReturnRows(sqlmock.NewRows([]string{"column_name"}).AddRow("issue_id"))
	mock.ExpectQuery(query).WithArgs("protected_mutation_permit_uses").
		WillReturnRows(sqlmock.NewRows([]string{"column_name"}).AddRow("audience").AddRow("replay_id"))
}

var protectedMutationReadinessColumns = []struct {
	table     string
	name      string
	dataType  string
	maxLength any
	nullable  string
}{
	{table: "protected_issue_records", name: "issue_id", dataType: "varchar", maxLength: int64(255), nullable: "NO"},
	{table: "protected_issue_records", name: "protection_class", dataType: "varchar", maxLength: int64(64), nullable: "NO"},
	{table: "protected_mutation_permit_uses", name: "audience", dataType: "varchar", maxLength: int64(255), nullable: "NO"},
	{table: "protected_mutation_permit_uses", name: "replay_id", dataType: "varchar", maxLength: int64(255), nullable: "NO"},
}

func expectProtectedMutationColumnShapes(mock sqlmock.Sqlmock) {
	expectProtectedMutationColumnShapesUntil(mock, -1, "", nil, "")
}

func expectProtectedMutationColumnShapesUntil(mock sqlmock.Sqlmock, malformedIndex int, dataType string, maxLength any, nullable string) {
	query := regexp.QuoteMeta(`SELECT data_type, character_maximum_length, is_nullable
		FROM information_schema.columns
		WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?`)
	for index, column := range protectedMutationReadinessColumns {
		shape := column
		if index == malformedIndex {
			shape.dataType = dataType
			shape.maxLength = maxLength
			shape.nullable = nullable
		}
		mock.ExpectQuery(query).WithArgs(column.table, column.name).
			WillReturnRows(sqlmock.NewRows([]string{"data_type", "character_maximum_length", "is_nullable"}).
				AddRow(shape.dataType, shape.maxLength, shape.nullable))
		if index == malformedIndex {
			return
		}
	}
}

func expectProtectedMutationTableProbes(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(regexp.QuoteMeta("SELECT issue_id, protection_class FROM protected_issue_records LIMIT 0")).
		WillReturnRows(sqlmock.NewRows([]string{"issue_id", "protection_class"}))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT audience, replay_id FROM protected_mutation_permit_uses LIMIT 0")).
		WillReturnRows(sqlmock.NewRows([]string{"audience", "replay_id"}))
}
