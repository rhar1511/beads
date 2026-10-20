package issueops

import (
	"context"
	"database/sql/driver"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	publicops "github.com/rhar1511/beads/issueops"
)

func expectNoProtectedRecords(mock sqlmock.Sqlmock, ids ...string) {
	ids = canonicalProtectedIDs(ids)
	args := make([]driver.Value, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	mock.ExpectQuery(regexp.QuoteMeta("SELECT issue_id FROM protected_issue_records WHERE protection_class <> '' AND issue_id IN (" + placeholders + ") ORDER BY issue_id")).
		WithArgs(args...).
		WillReturnRows(sqlmock.NewRows([]string{"issue_id"}))
}

type protectedMutationVerifierFunc func(context.Context, publicops.ProtectedMutationPermit, publicops.ProtectedMutationScope) (publicops.ProtectedMutationVerification, error)

func (f protectedMutationVerifierFunc) Verify(ctx context.Context, permit publicops.ProtectedMutationPermit, scope publicops.ProtectedMutationScope) (publicops.ProtectedMutationVerification, error) {
	return f(ctx, permit, scope)
}

func TestBatchApplyProtectedScopeCoversReachableLeafMutations(t *testing.T) {
	for _, test := range []struct {
		leaf string
		want bool
	}{
		{leaf: "issue.create", want: true},
		{leaf: "issue.update", want: true},
		{leaf: "issue.close", want: true},
		{leaf: "dependency.add", want: true},
		{leaf: "dependency.remove", want: true},
		{leaf: "issue.label_add", want: true},
		{leaf: "issue.label_remove", want: true},
		{leaf: "issue.persistence_move", want: true},
		// Batch UpdateItem has no Claim field, so issue.claim cannot be reached
		// through its update path.
		{leaf: "issue.claim", want: false},
		{leaf: "issue.comment", want: false},
	} {
		t.Run(test.leaf, func(t *testing.T) {
			if got := protectedOperationCovers("issue.batch_apply", test.leaf); got != test.want {
				t.Fatalf("protectedOperationCovers(issue.batch_apply, %s) = %v, want %v", test.leaf, got, test.want)
			}
		})
	}

	config := &protectedMutationConfig{verified: []publicops.ProtectedMutationScope{{
		Operation:   "issue.batch_apply",
		ResourceIDs: []string{"child", "new-parent", "old-parent"},
	}}}
	if !protectedAuthorizationCovers(config, "dependency.remove", []string{"old-parent"}) {
		t.Fatal("verified batch scope did not cover dependency.remove for its classified old parent")
	}
	if protectedAuthorizationCovers(config, "dependency.remove", []string{"outside-scope"}) {
		t.Fatal("verified batch scope covered dependency.remove for an unlisted protected record")
	}
}

func TestRequireProtectedMutationPassesExactScopeToVerifier(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectBegin()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT issue_id FROM protected_issue_records WHERE protection_class <> '' AND issue_id IN (?,?,?) ORDER BY issue_id")).
		WithArgs("alpha", "beta", "zeta").
		WillReturnRows(sqlmock.NewRows([]string{"issue_id"}).AddRow("alpha"))
	mock.ExpectExec(`INSERT INTO protected_mutation_permit_uses\s+\(audience, replay_id\) VALUES \(\?, \?\)`).
		WithArgs("workspace-audience", "unique-use-1").
		WillReturnResult(sqlmock.NewResult(1, 1))

	var got publicops.ProtectedMutationScope
	var gotPermit publicops.ProtectedMutationPermit
	verifier := protectedMutationVerifierFunc(func(_ context.Context, permit publicops.ProtectedMutationPermit, scope publicops.ProtectedMutationScope) (publicops.ProtectedMutationVerification, error) {
		got, gotPermit = scope, permit
		return publicops.ProtectedMutationVerification{ReplayID: "unique-use-1"}, nil
	})
	ctx := WithProtectedMutationVerifier(context.Background(), verifier, "workspace-audience", "project-a", "database-a")
	err = RequireProtectedMutation(ctx, tx, "signed-token", "issue.update", []string{"zeta", "alpha", "beta", "alpha"}, "canonical-digest", "")
	if err != nil {
		t.Fatalf("RequireProtectedMutation: %v", err)
	}
	if gotPermit != "signed-token" {
		t.Fatalf("permit = %q, want request permit", gotPermit)
	}
	if got.Audience != "workspace-audience" || got.ProjectID != "project-a" || got.Database != "database-a" ||
		got.Operation != "issue.update" || got.Digest != "canonical-digest" {
		t.Fatalf("scope = %#v, want exact authority binding, operation, and digest", got)
	}
	wantIDs := []string{"alpha", "beta", "zeta"}
	if len(got.ResourceIDs) != len(wantIDs) {
		t.Fatalf("scope resource IDs = %#v, want %#v", got.ResourceIDs, wantIDs)
	}
	for i := range wantIDs {
		if got.ResourceIDs[i] != wantIDs[i] {
			t.Fatalf("scope resource IDs = %#v, want %#v", got.ResourceIDs, wantIDs)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRequireProtectedMutationConsumesReplayIdentityInActiveTransaction(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectBegin()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT issue_id FROM protected_issue_records WHERE protection_class <> '' AND issue_id IN (?) ORDER BY issue_id")).
		WithArgs("protected").
		WillReturnRows(sqlmock.NewRows([]string{"issue_id"}).AddRow("protected"))
	mock.ExpectExec(`INSERT INTO protected_mutation_permit_uses\s+\(audience, replay_id\) VALUES \(\?, \?\)`).
		WithArgs("workspace-audience", "replay-1").
		WillReturnError(errors.New("duplicate key"))

	verifier := protectedMutationVerifierFunc(func(context.Context, publicops.ProtectedMutationPermit, publicops.ProtectedMutationScope) (publicops.ProtectedMutationVerification, error) {
		return publicops.ProtectedMutationVerification{ReplayID: "replay-1"}, nil
	})
	ctx := WithProtectedMutationVerifier(context.Background(), verifier, "workspace-audience", "project-a", "database-a")
	err = RequireProtectedMutation(ctx, tx, "permit", "issue.delete", []string{"protected"}, "digest", "")
	if !errors.Is(err, publicops.ErrProtectedRecord) {
		t.Fatalf("RequireProtectedMutation error = %v, want ErrProtectedRecord on replay", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A fresh-snapshot blocked-state repair must not erase the preceding write's
// permit-use flag before that write publishes its replay record to Dolt history.
func TestBlockedRecheckPreservesCommittedPermitPublication(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectBegin()
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT issue_id FROM protected_issue_records WHERE protection_class <> '' AND issue_id IN (?) ORDER BY issue_id")).
		WithArgs("protected").
		WillReturnRows(sqlmock.NewRows([]string{"issue_id"}).AddRow("protected"))
	mock.ExpectExec(`INSERT INTO protected_mutation_permit_uses\s+\(audience, replay_id\) VALUES \(\?, \?\)`).
		WithArgs("workspace-audience", "replay-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	verifier := protectedMutationVerifierFunc(func(context.Context, publicops.ProtectedMutationPermit, publicops.ProtectedMutationScope) (publicops.ProtectedMutationVerification, error) {
		return publicops.ProtectedMutationVerification{ReplayID: "replay-1"}, nil
	})
	ctx := WithProtectedMutationVerifier(t.Context(), verifier, "workspace-audience", "project-a", "database-a")
	if err := RequireProtectedMutation(ctx, tx, "permit", "issue.close", []string{"protected"}, "digest", ""); err != nil {
		t.Fatal(err)
	}
	mock.ExpectCommit()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	recheck, cancel := BlockedRecheckContext(ctx)
	defer cancel()
	ResetProtectedMutationAttempt(recheck)
	if !ProtectedMutationPermitUseWritten(ctx) {
		t.Fatal("post-commit recheck erased the committed write's permit publication obligation")
	}
	if ProtectedMutationPermitUseWritten(recheck) {
		t.Fatal("post-commit recheck inherited the preceding transaction's permit use")
	}
	if !ProtectedMutationAuthorityConfigured(recheck) {
		t.Fatal("post-commit recheck lost its store authority binding")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRequireProtectedMutationFailsClosedWithoutVerifier(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectBegin()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT issue_id FROM protected_issue_records WHERE protection_class <> '' AND issue_id IN (?) ORDER BY issue_id")).
		WithArgs("protected").
		WillReturnRows(sqlmock.NewRows([]string{"issue_id"}).AddRow("protected"))

	err = RequireProtectedMutation(context.Background(), tx, "", "issue.delete", []string{"protected"}, "", "")
	if !errors.Is(err, publicops.ErrProtectedRecord) {
		t.Fatalf("RequireProtectedMutation error = %v, want ErrProtectedRecord", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
