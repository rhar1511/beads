package uow

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/rhar1511/beads/internal/storage"
	"github.com/rhar1511/beads/internal/storage/domain"
	publicops "github.com/rhar1511/beads/issueops"
)

type notifyPatchUseCase struct {
	*notifyIssueUC
	wrote     bool
	err       error
	receipt   *publicops.RevisionTransitionPatchReceipt
	plan      storage.RevisionTransitionPatchPlan
	receiptID string
}

func (u *notifyPatchUseCase) TransitionPatch(_ context.Context, plan storage.RevisionTransitionPatchPlan) (publicops.RevisionTransitionPatchResult, bool, error) {
	u.plan = plan
	return publicops.RevisionTransitionPatchResult{Receipt: u.receipt, Applied: u.wrote}, u.wrote, u.err
}
func (u *notifyPatchUseCase) GetRevisionTransitionPatchReceipt(_ context.Context, id string) (*publicops.RevisionTransitionPatchReceipt, error) {
	u.receiptID = id
	return u.receipt, u.err
}

// Hooks must preserve the optional patch role, fire only after a committed
// change, and forward receipt reads without synthesizing another update.
func TestNotifyingRevisionPatchPreservesCapabilityAndCommitBoundary(t *testing.T) {
	refused := errors.New("patch refused")
	for _, tc := range []struct {
		name      string
		wrote     bool
		err       error
		commitErr error
		wantHooks int
	}{
		{name: "applied", wrote: true, wantHooks: 1},
		{name: "replay"},
		{name: "refused", err: refused},
		{name: "commit failure", wrote: true, commitErr: errors.New("commit refused")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNotifyFixture(t, seedIssue("bd-patch"))
			receipt := &publicops.RevisionTransitionPatchReceipt{ReceiptID: "patch-receipt"}
			inner := &notifyPatchUseCase{notifyIssueUC: f.issues, wrote: tc.wrote, err: tc.err, receipt: receipt}
			f.uow.issueUseCase = inner
			f.uow.commitErr = tc.commitErr
			uw := f.newUOW(t)
			defer uw.Close(t.Context())
			patcher, ok := uw.IssueUseCase().(domain.RevisionTransitionPatchUseCase)
			if !ok {
				t.Fatal("notifying wrapper hides revision patch capability")
			}
			plan := storage.RevisionTransitionPatchPlan{IssueID: "bd-patch", ReceiptID: "patch-receipt"}
			result, wrote, err := patcher.TransitionPatch(t.Context(), plan)
			if wrote != tc.wrote || !errors.Is(err, tc.err) || result.Receipt != receipt || !reflect.DeepEqual(inner.plan, plan) {
				t.Fatalf("patch result=%+v wrote=%v err=%v plan=%+v", result, wrote, err, inner.plan)
			}
			if len(f.runner.events()) != 0 {
				t.Fatal("patch fired before commit")
			}
			got, err := patcher.GetRevisionTransitionPatchReceipt(t.Context(), receipt.ReceiptID)
			if got != receipt || !errors.Is(err, tc.err) || inner.receiptID != receipt.ReceiptID {
				t.Fatalf("receipt=%+v err=%v id=%q", got, err, inner.receiptID)
			}
			if err := uw.Commit(t.Context(), "bd: patch"); !errors.Is(err, tc.commitErr) {
				t.Fatalf("commit=%v", err)
			}
			if got := len(f.runner.events()); got != tc.wantHooks {
				t.Fatalf("hooks=%d want=%d", got, tc.wantHooks)
			}
			if tc.wantHooks == 1 && f.runner.events()[0].id != "bd-patch" {
				t.Fatalf("hook=%+v", f.runner.events()[0])
			}
		})
	}
}
