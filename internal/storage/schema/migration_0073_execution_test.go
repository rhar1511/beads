package schema

import (
	"context"
	"database/sql/driver"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestValidateMigration0073ExecutionIdentityBinding(t *testing.T) {
	original, err := mainSource.files.ReadFile(mainSource.dir + "/" + migration0073Name)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name                  string
		cursor, dir, filename string
		version               int
		drift                 bool
	}{
		{"exact", mainSource.cursorTable, mainSource.dir, migration0073Name, 73, false},
		{"cursor", "ignored_schema_migrations", mainSource.dir, migration0073Name, 73, false},
		{"directory", mainSource.cursorTable, "migrations/ignored", migration0073Name, 73, false},
		{"filename", mainSource.cursorTable, mainSource.dir, "0073_other.up.sql", 73, false},
		{"version", mainSource.cursorTable, mainSource.dir, migration0073Name, 74, false},
		{"content", mainSource.cursorTable, mainSource.dir, migration0073Name, 73, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			src := mainSource
			src.cursorTable = test.cursor
			src.dir = test.dir
			body := append([]byte(nil), original...)
			if test.drift {
				body = append(body, []byte("\n-- drift")...)
			}
			before := string(body)
			err := validateMigration0073Execution(src, migrationFile{version: test.version, name: test.filename}, body)
			if (err == nil) != (test.name == "exact") {
				t.Fatalf("unexpected validation result: %v", err)
			}
			if string(body) != before {
				t.Fatal("validation mutated frozen bytes")
			}
		})
	}
}

func TestMigration0073RejectsPartialIncompatibleBeforeDDL(t *testing.T) {
	for _, column := range []string{"request_json", "receipt_json"} {
		for _, defect := range []string{"type", "nullable", "default", "extra"} {
			t.Run(column+"/"+defect, func(t *testing.T) {
				db, mock, err := sqlmock.New()
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				columnType, nullable, extra := "longtext", "YES", ""
				var defaultValue driver.Value
				switch defect {
				case "type":
					columnType = "text"
				case "nullable":
					nullable = "NO"
				case "default":
					defaultValue = "not-null-default"
				case "extra":
					extra = "VIRTUAL GENERATED"
				}
				mock.ExpectQuery("SELECT COLUMN_NAME, COLUMN_TYPE, IS_NULLABLE, COLUMN_DEFAULT, EXTRA").WillReturnRows(sqlmock.NewRows([]string{"COLUMN_NAME", "COLUMN_TYPE", "IS_NULLABLE", "COLUMN_DEFAULT", "EXTRA"}).AddRow(column, columnType, nullable, defaultValue, extra)).RowsWillBeClosed()
				err = execMigration0073(context.Background(), db)
				if err == nil || !strings.Contains(err.Error(), "incompatible definition") {
					t.Fatalf("unexpected refusal: %v", err)
				}
				if err := mock.ExpectationsWereMet(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
