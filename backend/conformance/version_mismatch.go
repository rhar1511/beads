package conformance

import (
	"errors"
	"testing"

	publicops "github.com/steveyegge/beads/issueops"
)

// Check the public error alongside each existing stale-write/no-mutation case.
// CLI callers need these exact transaction-owned tokens for their wire response.
func assertVersionMismatchTokens(t *testing.T, err error, id string, expected, current int64) {
	t.Helper()
	if !errors.Is(err, publicops.ErrVersionMismatch) {
		t.Fatalf("stale write: error = %v, want ErrVersionMismatch", err)
	}
	var mismatch *publicops.VersionMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("stale write: error %T lacks transaction-owned revision tokens: %v", err, err)
	}
	if mismatch.IssueID != id || mismatch.ExpectedVersion != expected || mismatch.CurrentVersion != current {
		t.Fatalf("stale write tokens = %+v, want id=%s expected=%d current=%d", mismatch, id, expected, current)
	}
}
