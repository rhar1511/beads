package uow

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
	publicops "github.com/steveyegge/beads/issueops"
)

type protectedBatchVerifierFunc func(context.Context, publicops.ProtectedMutationPermit, publicops.ProtectedMutationScope) (publicops.ProtectedMutationVerification, error)

func (f protectedBatchVerifierFunc) Verify(ctx context.Context, permit publicops.ProtectedMutationPermit, scope publicops.ProtectedMutationScope) (publicops.ProtectedMutationVerification, error) {
	return f(ctx, permit, scope)
}

func TestUOWProtectedBatchCreateLinkPermitAndReplay(t *testing.T) {
	ctx := context.Background()
	provider := newUOWRoleFixtureProvider(t, ctx, "pbatch")
	kit := newUOWRoleFixtureKit(provider, "pbatch")
	sourceID := "pbatch-source"
	if err := kit.CreateIssue(ctx, &types.Issue{
		ID: sourceID, Title: "source", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask,
	}, "seed"); err != nil {
		t.Fatalf("CreateIssue(source): %v", err)
	}
	applier, err := NewBatchApplier(provider)
	if err != nil {
		t.Fatalf("NewBatchApplier: %v", err)
	}
	count := func(query string, args ...any) int {
		t.Helper()
		var result int
		if err := kit.QueryScalar(ctx, query, args, &result); err != nil {
			t.Fatalf("query %q: %v", query, err)
		}
		return result
	}
	createLinkRequest := func(id, key string, permit publicops.ProtectedMutationPermit, protectionClass string) publicops.ApplyBatchRequest {
		return publicops.ApplyBatchRequest{
			Actor:           "trusted-caller",
			ProtectedPermit: permit,
			Items: []publicops.ApplyItem{
				{Kind: publicops.ItemCreate, Create: &publicops.CreateItem{
					Key: key,
					Issue: &types.Issue{
						ID: id, Title: "protected record", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask,
					},
					ProtectionClass: protectionClass,
				}},
				{Kind: publicops.ItemDepAdd, DepAdd: &publicops.DepAddItem{
					Source: publicops.Ref{ID: sourceID}, Target: publicops.Ref{Key: key}, Type: types.DepRelatesTo,
				}},
			},
		}
	}
	assertScope := func(request publicops.ApplyBatchRequest, wantIDs []string, replayID string, verifyErr error) publicops.ProtectedMutationVerifier {
		t.Helper()
		digest, err := publicops.ProtectedMutationDigest(request)
		if err != nil {
			t.Fatalf("ProtectedMutationDigest: %v", err)
		}
		return protectedBatchVerifierFunc(func(_ context.Context, permit publicops.ProtectedMutationPermit, scope publicops.ProtectedMutationScope) (publicops.ProtectedMutationVerification, error) {
			if permit != request.ProtectedPermit {
				t.Errorf("permit = %q, want the request's exact opaque permit %q", permit, request.ProtectedPermit)
			}
			if scope.Audience != "test-audience" || scope.Operation != "issue.batch_apply" {
				t.Errorf("scope audience/operation = %q/%q, want test-audience/issue.batch_apply", scope.Audience, scope.Operation)
			}
			if !equalProtectedBatchIDs(scope.ResourceIDs, wantIDs) {
				t.Errorf("scope resources = %v, want canonical exact set %v", scope.ResourceIDs, wantIDs)
			}
			if scope.Digest != digest {
				t.Errorf("scope digest = %q, want exact permit-cleared request digest %q", scope.Digest, digest)
			}
			if verifyErr != nil {
				return publicops.ProtectedMutationVerification{}, verifyErr
			}
			return publicops.ProtectedMutationVerification{ReplayID: replayID}, nil
		})
	}

	// Ordinary create-plus-link remains available without a permit. Metadata,
	// labels and title that resemble classification data do not classify it.
	ordinary := createLinkRequest("pbatch-ordinary", "ordinary", "", "")
	ordinary.Items[0].Create.Issue.Title = "protected source"
	ordinary.Items[0].Create.Issue.Labels = []string{"protected", "source"}
	ordinary.Items[0].Create.Issue.Metadata = json.RawMessage(`{"protection_class":"source"}`)
	if _, err := applier.ApplyBatch(ctx, ordinary); err != nil {
		t.Fatalf("ordinary create and link without permit: %v", err)
	}
	if got := count("SELECT COUNT(*) FROM protected_issue_records WHERE issue_id = ?", "pbatch-ordinary"); got != 0 {
		t.Errorf("content-only protected classification count = %d, want 0", got)
	}
	if got := count("SELECT COUNT(*) FROM dependencies WHERE issue_id = ? AND depends_on_issue_id = ?", sourceID, "pbatch-ordinary"); got != 1 {
		t.Errorf("ordinary dependency edge count = %d, want 1", got)
	}

	// An explicit class with no permit is refused before either row or edge is
	// written, even when no verifier is configured on the request context.
	noPermit := createLinkRequest("pbatch-no-permit", "no_permit", "", "source")
	if _, err := applier.ApplyBatch(ctx, noPermit); !errors.Is(err, publicops.ErrProtectedRecord) {
		t.Fatalf("classified create without permit error = %v, want ErrProtectedRecord", err)
	}
	if got := count("SELECT COUNT(*) FROM issues WHERE id = ?", "pbatch-no-permit"); got != 0 {
		t.Errorf("no-permit issue count = %d, want 0", got)
	}
	if got := count("SELECT COUNT(*) FROM protected_issue_records WHERE issue_id = ?", "pbatch-no-permit"); got != 0 {
		t.Errorf("no-permit classification count = %d, want 0", got)
	}
	if got := count("SELECT COUNT(*) FROM dependencies WHERE issue_id = ? AND depends_on_issue_id = ?", sourceID, "pbatch-no-permit"); got != 0 {
		t.Errorf("no-permit dependency edge count = %d, want 0", got)
	}

	// A verifier refusal for this exact operation, resource set and digest also
	// leaves no classification, row, edge, or replay-use record.
	wrong := createLinkRequest("pbatch-wrong", "wrong", "wrong-permit", "source")
	wrongCtx := issueops.WithProtectedMutationVerifier(ctx,
		assertScope(wrong, []string{sourceID, "pbatch-wrong"}, "pbatch-wrong-replay", errors.New("signature mismatch")),
		"test-audience")
	if _, err := applier.ApplyBatch(wrongCtx, wrong); !errors.Is(err, publicops.ErrProtectedRecord) {
		t.Fatalf("wrong permit error = %v, want ErrProtectedRecord", err)
	}
	if got := count("SELECT COUNT(*) FROM issues WHERE id = ?", "pbatch-wrong"); got != 0 {
		t.Errorf("wrong-permit issue count = %d, want 0", got)
	}
	if got := count("SELECT COUNT(*) FROM protected_issue_records WHERE issue_id = ?", "pbatch-wrong"); got != 0 {
		t.Errorf("wrong-permit classification count = %d, want 0", got)
	}
	if got := count("SELECT COUNT(*) FROM dependencies WHERE issue_id = ? AND depends_on_issue_id = ?", sourceID, "pbatch-wrong"); got != 0 {
		t.Errorf("wrong-permit dependency edge count = %d, want 0", got)
	}
	if got := count("SELECT COUNT(*) FROM protected_mutation_permit_uses WHERE audience = ? AND replay_id = ?", "test-audience", "pbatch-wrong-replay"); got != 0 {
		t.Errorf("wrong-permit replay-use count = %d, want 0", got)
	}

	request := createLinkRequest("pbatch-protected", "protected", "valid-permit", "source")
	verifier := assertScope(request, []string{"pbatch-protected", sourceID}, "pbatch-protected-replay", nil)
	protectedCtx := issueops.WithProtectedMutationVerifier(ctx, verifier, "test-audience")
	if _, err := applier.ApplyBatch(protectedCtx, request); err != nil {
		t.Fatalf("protected create and link: %v", err)
	}
	if got := count("SELECT COUNT(*) FROM issues WHERE id = ?", "pbatch-protected"); got != 1 {
		t.Errorf("protected issue count = %d, want 1", got)
	}
	if got := count("SELECT COUNT(*) FROM protected_issue_records WHERE issue_id = ? AND protection_class = ?", "pbatch-protected", "source"); got != 1 {
		t.Errorf("explicit classification count = %d, want 1", got)
	}
	if got := count("SELECT COUNT(*) FROM dependencies WHERE issue_id = ? AND depends_on_issue_id = ?", sourceID, "pbatch-protected"); got != 1 {
		t.Errorf("protected dependency edge count = %d, want 1", got)
	}
	if got := count("SELECT COUNT(*) FROM protected_mutation_permit_uses WHERE audience = ? AND replay_id = ?", "test-audience", "pbatch-protected-replay"); got != 1 {
		t.Errorf("protected replay-use count = %d, want 1", got)
	}

	// Reusing the same verifier replay identity in a new transaction is refused;
	// the already committed row and edge stay unchanged.
	if _, err := applier.ApplyBatch(protectedCtx, request); !errors.Is(err, publicops.ErrProtectedRecord) {
		t.Fatalf("replayed protected batch error = %v, want ErrProtectedRecord", err)
	}
	if got := count("SELECT COUNT(*) FROM issues WHERE id = ?", "pbatch-protected"); got != 1 {
		t.Errorf("protected issue count after replay = %d, want 1", got)
	}
	if got := count("SELECT COUNT(*) FROM dependencies WHERE issue_id = ? AND depends_on_issue_id = ?", sourceID, "pbatch-protected"); got != 1 {
		t.Errorf("protected edge count after replay = %d, want 1", got)
	}
	if got := count("SELECT COUNT(*) FROM protected_mutation_permit_uses WHERE audience = ? AND replay_id = ?", "test-audience", "pbatch-protected-replay"); got != 1 {
		t.Errorf("protected replay-use count after replay = %d, want 1", got)
	}

	// The dependency is inserted after the create; a later item then fails.
	// The unit of work must roll back the row, classification, edge and permit
	// replay consumption together.
	rollback := createLinkRequest("pbatch-rollback", "rollback", "rollback-permit", "source")
	rollback.Items = append(rollback.Items, publicops.ApplyItem{
		Kind:  publicops.ItemClose,
		Close: &publicops.CloseItem{Target: publicops.Ref{ID: "pbatch-missing-close"}},
	})
	rollbackCtx := issueops.WithProtectedMutationVerifier(ctx,
		assertScope(rollback, []string{"pbatch-missing-close", "pbatch-rollback", sourceID}, "pbatch-rollback-replay", nil),
		"test-audience")
	if _, err := applier.ApplyBatch(rollbackCtx, rollback); err == nil {
		t.Fatal("batch with a missing later close target succeeded, want rollback")
	}
	if got := count("SELECT COUNT(*) FROM issues WHERE id = ?", "pbatch-rollback"); got != 0 {
		t.Errorf("rolled-back issue count = %d, want 0", got)
	}
	if got := count("SELECT COUNT(*) FROM protected_issue_records WHERE issue_id = ?", "pbatch-rollback"); got != 0 {
		t.Errorf("rolled-back classification count = %d, want 0", got)
	}
	if got := count("SELECT COUNT(*) FROM dependencies WHERE issue_id = ? AND depends_on_issue_id = ?", sourceID, "pbatch-rollback"); got != 0 {
		t.Errorf("rolled-back dependency edge count = %d, want 0", got)
	}
	if got := count("SELECT COUNT(*) FROM protected_mutation_permit_uses WHERE audience = ? AND replay_id = ?", "test-audience", "pbatch-rollback-replay"); got != 0 {
		t.Errorf("rolled-back replay-use count = %d, want 0", got)
	}
}

func TestUOWProtectedBatchNoOpCommitsPermitUseAndRejectsReplay(t *testing.T) {
	ctx := context.Background()
	provider := newUOWRoleFixtureProvider(t, ctx, "pbnoop")
	kit := newUOWRoleFixtureKit(provider, "pbnoop")
	issueID := "pbnoop-record"
	const title = "unchanged protected issue"
	if err := kit.CreateIssue(ctx, &types.Issue{
		ID: issueID, Title: title, Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask,
	}, "seed"); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	classUOWIssue(t, ctx, provider, issueID)
	applier, err := NewBatchApplier(provider)
	if err != nil {
		t.Fatalf("NewBatchApplier: %v", err)
	}
	request := publicops.ApplyBatchRequest{
		Actor:           "trusted-caller",
		ProtectedPermit: "no-op-permit",
		Items: []publicops.ApplyItem{{
			Kind: publicops.ItemUpdate,
			Update: &publicops.UpdateItem{
				Target: publicops.Ref{ID: issueID},
				Patch:  publicops.IssuePatch{Title: publicops.Field[string]{Set: true, Value: title}},
			},
		}},
	}
	digest, err := publicops.ProtectedMutationDigest(request)
	if err != nil {
		t.Fatalf("ProtectedMutationDigest: %v", err)
	}
	verifier := protectedBatchVerifierFunc(func(_ context.Context, permit publicops.ProtectedMutationPermit, scope publicops.ProtectedMutationScope) (publicops.ProtectedMutationVerification, error) {
		if permit != request.ProtectedPermit {
			t.Errorf("permit = %q, want %q", permit, request.ProtectedPermit)
		}
		if scope.Audience != "test-audience" || scope.Operation != "issue.batch_apply" {
			t.Errorf("scope audience/operation = %q/%q, want test-audience/issue.batch_apply", scope.Audience, scope.Operation)
		}
		if !equalProtectedBatchIDs(scope.ResourceIDs, []string{issueID}) {
			t.Errorf("scope resources = %v, want [%s]", scope.ResourceIDs, issueID)
		}
		if scope.Digest != digest {
			t.Errorf("scope digest = %q, want %q", scope.Digest, digest)
		}
		return publicops.ProtectedMutationVerification{ReplayID: "pbnoop-replay"}, nil
	})
	protectedCtx := issueops.WithProtectedMutationVerifier(ctx, verifier, "test-audience")
	result, err := applier.ApplyBatch(protectedCtx, request)
	if err != nil {
		t.Fatalf("protected semantic no-op batch: %v", err)
	}
	if len(result.Items) != 1 || result.Items[0].Changed {
		t.Fatalf("no-op batch items = %+v, want one item with Changed=false", result.Items)
	}
	var permitUses int
	if err := kit.QueryScalar(ctx,
		"SELECT COUNT(*) FROM protected_mutation_permit_uses WHERE audience = ? AND replay_id = ?",
		[]any{"test-audience", "pbnoop-replay"}, &permitUses); err != nil {
		t.Fatalf("count permit uses after no-op: %v", err)
	}
	if permitUses != 1 {
		t.Fatalf("permit-use rows after no-op = %d, want 1", permitUses)
	}
	readDomainState := func() (string, int64) {
		t.Helper()
		var gotTitle string
		if err := kit.QueryScalar(ctx, "SELECT title FROM issues WHERE id = ?", []any{issueID}, &gotTitle); err != nil {
			t.Fatalf("read issue title: %v", err)
		}
		var rowLock int64
		if err := kit.QueryScalar(ctx, "SELECT row_lock FROM issues WHERE id = ?", []any{issueID}, &rowLock); err != nil {
			t.Fatalf("read issue row lock: %v", err)
		}
		return gotTitle, rowLock
	}
	afterSuccessTitle, afterSuccessLock := readDomainState()
	if afterSuccessTitle != title {
		t.Fatalf("issue title after no-op = %q, want %q", afterSuccessTitle, title)
	}
	if _, err := applier.ApplyBatch(protectedCtx, request); !errors.Is(err, publicops.ErrProtectedRecord) {
		t.Fatalf("replayed no-op protected batch error = %v, want ErrProtectedRecord", err)
	}
	afterReplayTitle, afterReplayLock := readDomainState()
	if afterReplayTitle != afterSuccessTitle || afterReplayLock != afterSuccessLock {
		t.Errorf("replay changed domain state: before=(%q,%d), after=(%q,%d)", afterSuccessTitle, afterSuccessLock, afterReplayTitle, afterReplayLock)
	}
	if err := kit.QueryScalar(ctx,
		"SELECT COUNT(*) FROM protected_mutation_permit_uses WHERE audience = ? AND replay_id = ?",
		[]any{"test-audience", "pbnoop-replay"}, &permitUses); err != nil {
		t.Fatalf("count permit uses after replay: %v", err)
	}
	if permitUses != 1 {
		t.Errorf("permit-use rows after replay = %d, want 1", permitUses)
	}
}

func TestUOWProtectedBatchReceiptReplaySkipsPermitRevalidation(t *testing.T) {
	ctx := context.Background()
	provider := newUOWRoleFixtureProvider(t, ctx, "pbrec")
	kit := newUOWRoleFixtureKit(provider, "pbrec")
	applier, err := NewBatchApplier(provider)
	if err != nil {
		t.Fatalf("NewBatchApplier: %v", err)
	}
	request := publicops.ApplyBatchRequest{
		Actor:           "trusted-caller",
		ProtectedPermit: "permit-first",
		ReceiptID:       "pbrec-protected-receipt",
		Items: []publicops.ApplyItem{{
			Kind: publicops.ItemCreate,
			Create: &publicops.CreateItem{
				Issue: &types.Issue{
					ID: "pbrec-protected", Title: "protected receipt", Status: types.StatusOpen,
					Priority: 2, IssueType: types.TypeTask,
				},
				ProtectionClass: "source",
			},
		}},
	}
	var verifierCalls int
	verifier := protectedBatchVerifierFunc(func(_ context.Context, permit publicops.ProtectedMutationPermit, scope publicops.ProtectedMutationScope) (publicops.ProtectedMutationVerification, error) {
		verifierCalls++
		if verifierCalls != 1 {
			return publicops.ProtectedMutationVerification{}, errors.New("receipt replay revalidated the permit")
		}
		if permit != request.ProtectedPermit {
			t.Errorf("first permit = %q, want %q", permit, request.ProtectedPermit)
		}
		if scope.Operation != "issue.batch_apply" || scope.Audience != "test-audience" {
			t.Errorf("protected scope = %#v, want batch apply for test-audience", scope)
		}
		return publicops.ProtectedMutationVerification{ReplayID: "pbrec-replay-use"}, nil
	})
	protectedCtx := issueops.WithProtectedMutationVerifier(ctx, verifier, "test-audience")
	first, err := applier.ApplyBatch(protectedCtx, request)
	if err != nil {
		t.Fatalf("first receipt-bearing protected batch: %v", err)
	}
	if first.Replayed {
		t.Fatal("first receipt-bearing protected batch was marked replayed")
	}
	var uses int
	if err := kit.QueryScalar(ctx,
		"SELECT COUNT(*) FROM protected_mutation_permit_uses WHERE audience = ? AND replay_id = ?",
		[]any{"test-audience", "pbrec-replay-use"}, &uses); err != nil {
		t.Fatalf("count first permit use: %v", err)
	}
	if uses != 1 {
		t.Fatalf("first protected batch permit-use count = %d, want 1", uses)
	}

	// The permit itself is deliberately changed: it is authorization material,
	// not part of the receipt's canonical operation digest.
	retry := request
	retry.ProtectedPermit = "permit-that-must-not-be-checked-again"
	replayed, err := applier.ApplyBatch(protectedCtx, retry)
	if err != nil {
		t.Fatalf("receipt replay after permit consumption: %v", err)
	}
	if !replayed.Replayed {
		t.Fatal("protected receipt replay was not marked replayed")
	}
	want := first
	want.Replayed = true
	if !reflect.DeepEqual(replayed, want) {
		t.Errorf("protected replay result differs from original:\n got %#v\nwant %#v", replayed, want)
	}
	if verifierCalls != 1 {
		t.Errorf("protected verifier called %d times, want once", verifierCalls)
	}
	if err := kit.QueryScalar(ctx,
		"SELECT COUNT(*) FROM protected_mutation_permit_uses WHERE audience = ? AND replay_id = ?",
		[]any{"test-audience", "pbrec-replay-use"}, &uses); err != nil {
		t.Fatalf("recount permit uses after replay: %v", err)
	}
	if uses != 1 {
		t.Errorf("permit-use count after receipt replay = %d, want 1", uses)
	}
}

func TestUOWProtectedBatchReparentScopesTheOldParent(t *testing.T) {
	ctx := context.Background()
	provider := newUOWRoleFixtureProvider(t, ctx, "pbparent")
	kit := newUOWRoleFixtureKit(provider, "pbparent")
	oldParentID := "pbparent-old"
	secondOldParentID := "pbparent-old-2"
	newParentID := "pbparent-new"
	childID := "pbparent-child"
	for _, id := range []string{oldParentID, secondOldParentID, newParentID, childID} {
		if err := kit.CreateIssue(ctx, &types.Issue{
			ID: id, Title: id, Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask,
		}, "seed"); err != nil {
			t.Fatalf("CreateIssue(%s): %v", id, err)
		}
	}
	if err := kit.AddDependency(ctx, &types.Dependency{
		IssueID: childID, DependsOnID: oldParentID, Type: types.DepParentChild,
	}, "seed"); err != nil {
		t.Fatalf("AddDependency(old parent): %v", err)
	}
	if err := kit.AddDependency(ctx, &types.Dependency{
		IssueID: childID, DependsOnID: secondOldParentID, Type: types.DepParentChild,
	}, "seed"); err != nil {
		t.Fatalf("AddDependency(second old parent): %v", err)
	}
	classUOWIssue(t, ctx, provider, oldParentID)
	applier, err := NewBatchApplier(provider)
	if err != nil {
		t.Fatalf("NewBatchApplier: %v", err)
	}
	request := publicops.ApplyBatchRequest{
		Actor: "trusted-caller",
		Items: []publicops.ApplyItem{{
			Kind: publicops.ItemUpdate,
			Update: &publicops.UpdateItem{
				Target: publicops.Ref{ID: childID},
				Patch:  publicops.IssuePatch{ParentID: publicops.Field[string]{Set: true, Value: newParentID}},
			},
		}},
	}
	if _, err := applier.ApplyBatch(ctx, request); !errors.Is(err, publicops.ErrProtectedRecord) {
		t.Fatalf("reparent without permit error = %v, want ErrProtectedRecord from the classified old parent", err)
	}
	assertParentEdge := func(parentID string, want int) {
		t.Helper()
		var count int
		if err := kit.QueryScalar(ctx,
			"SELECT COUNT(*) FROM dependencies WHERE issue_id = ? AND depends_on_issue_id = ? AND type = 'parent-child'",
			[]any{childID, parentID}, &count); err != nil {
			t.Fatalf("count parent edge to %s: %v", parentID, err)
		}
		if count != want {
			t.Errorf("parent edge to %s count = %d, want %d", parentID, count, want)
		}
	}
	assertParentEdge(oldParentID, 1)
	assertParentEdge(secondOldParentID, 1)
	assertParentEdge(newParentID, 0)

	request.ProtectedPermit = "reparent-permit"
	digest, err := publicops.ProtectedMutationDigest(request)
	if err != nil {
		t.Fatalf("ProtectedMutationDigest: %v", err)
	}
	verified := false
	verifier := protectedBatchVerifierFunc(func(_ context.Context, permit publicops.ProtectedMutationPermit, scope publicops.ProtectedMutationScope) (publicops.ProtectedMutationVerification, error) {
		verified = true
		if permit != request.ProtectedPermit {
			t.Errorf("permit = %q, want %q", permit, request.ProtectedPermit)
		}
		if scope.Operation != "issue.batch_apply" || scope.Audience != "test-audience" {
			t.Errorf("scope audience/operation = %q/%q, want test-audience/issue.batch_apply", scope.Audience, scope.Operation)
		}
		if !equalProtectedBatchIDs(scope.ResourceIDs, []string{childID, newParentID, oldParentID, secondOldParentID}) {
			t.Errorf("scope resources = %v, want child, new parent, and both old parents", scope.ResourceIDs)
		}
		if scope.Digest != digest {
			t.Errorf("scope digest = %q, want permit-cleared request digest %q", scope.Digest, digest)
		}
		return publicops.ProtectedMutationVerification{ReplayID: "pbparent-reparent-replay"}, nil
	})
	protectedCtx := issueops.WithProtectedMutationVerifier(ctx, verifier, "test-audience")
	if _, err := applier.ApplyBatch(protectedCtx, request); err != nil {
		t.Fatalf("reparent with permit covering old parent: %v", err)
	}
	if !verified {
		t.Fatal("protected mutation verifier was not called")
	}
	assertParentEdge(oldParentID, 0)
	assertParentEdge(secondOldParentID, 0)
	assertParentEdge(newParentID, 1)
}

func equalProtectedBatchIDs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
