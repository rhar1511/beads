package schema

import (
	"strings"
	"testing"
)

func TestMigration0068ProvidesDurableTransitionReceipts(t *testing.T) {
	if got := LatestVersion(); got < 68 {
		t.Fatalf("latest schema version = %d, want at least 68", got)
	}
	up, err := MigrationSQL("0068_issue_transition_receipts.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"CREATE TABLE IF NOT EXISTS issue_transition_receipts",
		"receipt_id VARCHAR(255) NOT NULL PRIMARY KEY",
		"issue_id VARCHAR(255) NOT NULL",
		"expected_version BIGINT NOT NULL",
		"to_version BIGINT NOT NULL",
		"expected_json LONGTEXT NULL",
		"value_json LONGTEXT NULL",
	} {
		if !strings.Contains(up, required) {
			t.Errorf("receipt migration omits %q", required)
		}
	}
}
