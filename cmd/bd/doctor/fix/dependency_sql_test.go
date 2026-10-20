package fix

import (
	"testing"

	"github.com/rhar1511/beads/internal/storage/issueops"
)

func TestFixDependencyTargetExprMatchesCanonical(t *testing.T) {
	if fixDependencyTargetExpr != issueops.DepTargetExpr {
		t.Fatalf("fixDependencyTargetExpr = %q, want %q", fixDependencyTargetExpr, issueops.DepTargetExpr)
	}
}
