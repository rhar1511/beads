//go:build cgo

package embeddeddolt_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/embeddeddolt"
	storageissueops "github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
	publicops "github.com/steveyegge/beads/issueops"
)

type revisionPatchTestVerifier struct{}

func (revisionPatchTestVerifier) Verify(_ context.Context, permit publicops.ProtectedMutationPermit, scope publicops.ProtectedMutationScope) (publicops.ProtectedMutationVerification, error) {
	if permit == "" || scope.Audience != "test-audience" || scope.ProjectID != "test-project" || scope.Database != "test-db" ||
		scope.Operation != "issue.revision_transition_patch" || len(scope.ResourceIDs) != 1 || scope.Digest == "" {
		return publicops.ProtectedMutationVerification{}, errors.New("unexpected patch permit scope")
	}
	return publicops.ProtectedMutationVerification{ReplayID: string(permit)}, nil
}

func revisionPatchContext(ctx context.Context) context.Context {
	return storageissueops.WithProtectedMutationVerifier(ctx, revisionPatchTestVerifier{}, "test-audience", "test-project", "test-db")
}

func revisionPatchRequest(t *testing.T, parent publicops.RevisionTransitionReceipt, receiptID string) publicops.RevisionTransitionPatchRequest {
	t.Helper()
	digest, err := publicops.RevisionTransitionReceiptDigest(parent)
	if err != nil {
		t.Fatalf("RevisionTransitionReceiptDigest: %v", err)
	}
	return publicops.RevisionTransitionPatchRequest{
		ProtectedPermit:    publicops.ProtectedMutationPermit("patch-permit-" + receiptID),
		ReceiptID:          receiptID,
		IssueID:            parent.IssueID,
		Scope:              parent.Scope,
		Kind:               "route",
		Actor:              "revision-test",
		ExpectedVersion:    parent.ToVersion,
		PriorReceiptID:     parent.ReceiptID,
		PriorReceiptDigest: digest,
		Patch: publicops.RevisionTransitionIssuePatch{
			Metadata: []publicops.RevisionTransitionMetadataPatch{{
				Key: "gc_route", Expected: transitionJSON("pending"), Value: transitionJSON("ready"),
			}},
		},
	}
}

func createPatchParentReceipt(t *testing.T, te *testEnv, ctx context.Context, issue *types.Issue, revision int64, receiptID string) publicops.RevisionTransitionReceipt {
	t.Helper()
	role, err := te.store.RevisionTransition()
	if err != nil {
		t.Fatalf("RevisionTransition: %v", err)
	}
	result, err := role.TransitionMetadata(ctx, transitionRequest(receiptID, issue.ID, revision, nil, transitionJSON("admitted")))
	if err != nil || !result.Applied || result.Receipt == nil {
		t.Fatalf("TransitionMetadata parent = %+v, %v", result, err)
	}
	return *result.Receipt
}

func patcherFor(t *testing.T, te *testEnv) publicops.RevisionTransitionPatcher {
	t.Helper()
	role, err := te.store.RevisionTransition()
	if err != nil {
		t.Fatalf("RevisionTransition: %v", err)
	}
	patcher, ok := role.(publicops.RevisionTransitionPatcher)
	if !ok {
		t.Fatal("revision transition role does not implement patch capability")
	}
	return patcher
}

func TestRevisionTransitionPatchAppliesAllFieldsAndReplaysAfterRestart(t *testing.T) {
	skipUnlessEmbeddedDolt(t)
	te := newTestEnv(t, "rpa")
	ctx := t.Context()
	issue := &types.Issue{
		Title: "atomic patch fixture", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask,
		Assignee: "", Metadata: json.RawMessage(`{"gc_route":"pending"}`), Labels: []string{"keep", "old"},
	}
	if err := te.store.CreateIssue(ctx, issue, "revision-test"); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	var initial int64
	te.queryScalar(t, ctx, "SELECT row_lock FROM issues WHERE id = ?", []any{issue.ID}, &initial)
	parent := createPatchParentReceipt(t, te, ctx, issue, initial, "patch-parent-1")
	request := revisionPatchRequest(t, parent, "patch-apply-1")
	workflow := json.RawMessage(`{"name":"build"}`)
	request.Patch = publicops.RevisionTransitionIssuePatch{
		Metadata: []publicops.RevisionTransitionMetadataPatch{
			{Key: "gc_route", Expected: transitionJSON("pending"), Value: transitionJSON("ready")},
			{Key: "gc_workflow", Value: &workflow},
		},
		Status:   &publicops.RevisionTransitionStringPatch{Expected: "open", Value: "in_progress"},
		Assignee: &publicops.RevisionTransitionStringPatch{Expected: "", Value: "worker-1"},
		Labels:   &publicops.RevisionTransitionLabelsPatch{Expected: []string{"old", "keep"}, Value: []string{"new", "keep"}},
	}
	patcher := patcherFor(t, te)
	result, err := patcher.TransitionPatch(revisionPatchContext(ctx), request)
	if err != nil {
		t.Fatalf("TransitionPatch: %v", err)
	}
	if !result.Applied || result.Replayed || result.Receipt == nil {
		t.Fatalf("TransitionPatch result = %+v, want applied receipt", result)
	}
	receipt := result.Receipt
	if receipt.ExpectedVersion != parent.ToVersion || receipt.ToVersion == 0 || receipt.ToVersion == parent.ToVersion || receipt.PriorReceiptID != parent.ReceiptID || receipt.PriorReceiptDigest != request.PriorReceiptDigest {
		t.Fatalf("patch receipt = %+v, want exact parent and distinct server revision", receipt)
	}
	storedIssue, err := te.store.GetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("GetIssue after patch: %v", err)
	}
	if storedIssue.RowVersion != receipt.ToVersion || storedIssue.Status != types.StatusInProgress || storedIssue.Assignee != "worker-1" ||
		!jsonValuesEqual(t, revisionTransitionMetadataValue(t, storedIssue.Metadata, "gc_route"), "ready") || !jsonValuesEqual(t, revisionTransitionMetadataValue(t, storedIssue.Metadata, "gc_workflow"), map[string]interface{}{"name": "build"}) {
		t.Fatalf("source after patch = %+v, receipt = %+v", storedIssue, receipt)
	}
	if got := storedIssue.Labels; len(got) != 2 || got[0] != "keep" || got[1] != "new" {
		t.Fatalf("labels after patch = %v, want [keep new]", got)
	}
	var rowLock int64
	te.queryScalar(t, ctx, "SELECT row_lock FROM issues WHERE id = ?", []any{issue.ID}, &rowLock)
	if rowLock != receipt.ToVersion {
		t.Fatalf("persisted row_lock = %d, receipt to_version = %d", rowLock, receipt.ToVersion)
	}

	storedReceipt, err := patcher.GetPatchReceipt(ctx, request.ReceiptID)
	if err != nil || storedReceipt == nil || storedReceipt.ToVersion != receipt.ToVersion {
		t.Fatalf("GetPatchReceipt = %+v, %v", storedReceipt, err)
	}
	legacyRole, err := te.store.RevisionTransition()
	if err != nil {
		t.Fatalf("RevisionTransition for legacy receipt read: %v", err)
	}
	if _, err := legacyRole.GetReceipt(ctx, request.ReceiptID); !errors.Is(err, publicops.ErrTransitionReceiptConflict) {
		t.Fatalf("legacy GetReceipt for patch ID = %v, want typed conflict", err)
	}
	if err := te.store.UpdateIssue(ctx, issue.ID, map[string]interface{}{"title": "later edit"}, "revision-test"); err != nil {
		t.Fatalf("UpdateIssue after patch: %v", err)
	}
	var afterLaterEdit int64
	te.queryScalar(t, ctx, "SELECT row_lock FROM issues WHERE id = ?", []any{issue.ID}, &afterLaterEdit)
	if afterLaterEdit == receipt.ToVersion {
		t.Fatal("later source mutation did not advance the revision")
	}

	if err := te.store.Close(); err != nil {
		t.Fatalf("close store before restart recovery: %v", err)
	}
	reopened, err := embeddeddolt.Open(ctx, filepath.Dir(te.dataDir), te.database, "main")
	if err != nil {
		t.Fatalf("reopen store for recovery: %v", err)
	}
	te.store = reopened
	patcher = patcherFor(t, te)
	recovered, err := patcher.GetPatchReceipt(ctx, request.ReceiptID)
	if err != nil || recovered == nil || recovered.ToVersion != receipt.ToVersion {
		t.Fatalf("receipt after restart = %+v, %v", recovered, err)
	}
	replay, err := patcher.TransitionPatch(ctx, request)
	if err != nil || !replay.Applied || !replay.Replayed || replay.Receipt == nil || replay.Receipt.ToVersion != receipt.ToVersion {
		t.Fatalf("exact replay after restart = %+v, %v", replay, err)
	}
	var afterReplay int64
	te.queryScalar(t, ctx, "SELECT row_lock FROM issues WHERE id = ?", []any{issue.ID}, &afterReplay)
	if afterReplay != afterLaterEdit {
		t.Fatalf("row_lock after exact replay = %d, want unchanged %d", afterReplay, afterLaterEdit)
	}
	changed := request
	changed.Patch = request.Patch
	changed.Patch.Status = &publicops.RevisionTransitionStringPatch{Expected: "open", Value: "blocked"}
	if _, err := patcher.TransitionPatch(revisionPatchContext(ctx), changed); !errors.Is(err, publicops.ErrTransitionReceiptConflict) {
		t.Fatalf("same ID/different patch error = %v, want receipt conflict", err)
	}
}

func TestRevisionTransitionPatchRefusesStaleAndFieldPreconditionsWithoutPartialWrites(t *testing.T) {
	skipUnlessEmbeddedDolt(t)
	tests := []struct {
		name      string
		mutate    func(*testEnv, context.Context, *types.Issue, publicops.RevisionTransitionReceipt, *publicops.RevisionTransitionPatchRequest)
		wantError error
	}{
		{
			name: "stale source revision",
			mutate: func(te *testEnv, ctx context.Context, issue *types.Issue, _ publicops.RevisionTransitionReceipt, _ *publicops.RevisionTransitionPatchRequest) {
				if err := te.store.UpdateIssue(ctx, issue.ID, map[string]interface{}{"title": "intervening edit"}, "revision-test"); err != nil {
					t.Fatalf("UpdateIssue: %v", err)
				}
			},
			wantError: storage.ErrVersionMismatch,
		},
		{
			name: "metadata expected value mismatch",
			mutate: func(_ *testEnv, _ context.Context, _ *types.Issue, _ publicops.RevisionTransitionReceipt, request *publicops.RevisionTransitionPatchRequest) {
				request.Patch.Metadata[0].Expected = transitionJSON("wrong")
			},
			wantError: publicops.ErrRevisionPatchPreconditionFailed,
		},
		{
			name: "labels expected set mismatch",
			mutate: func(_ *testEnv, _ context.Context, _ *types.Issue, _ publicops.RevisionTransitionReceipt, request *publicops.RevisionTransitionPatchRequest) {
				request.Patch.Labels = &publicops.RevisionTransitionLabelsPatch{Expected: []string{"not-current"}, Value: []string{"next"}}
			},
			wantError: publicops.ErrRevisionPatchPreconditionFailed,
		},
	}
	for index, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t, fmt.Sprintf("rpf%d", index))
			ctx := t.Context()
			issue := &types.Issue{
				Title: "patch refusal fixture", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask,
				Metadata: json.RawMessage(`{"gc_route":"pending"}`), Labels: []string{"old"},
			}
			if err := te.store.CreateIssue(ctx, issue, "revision-test"); err != nil {
				t.Fatalf("CreateIssue: %v", err)
			}
			var initial int64
			te.queryScalar(t, ctx, "SELECT row_lock FROM issues WHERE id = ?", []any{issue.ID}, &initial)
			parent := createPatchParentReceipt(t, te, ctx, issue, initial, fmt.Sprintf("patch-parent-%d", index))
			request := revisionPatchRequest(t, parent, fmt.Sprintf("patch-refused-%d", index))
			request.Patch.Status = &publicops.RevisionTransitionStringPatch{Expected: "open", Value: "in_progress"}
			request.Patch.Assignee = &publicops.RevisionTransitionStringPatch{Expected: "", Value: "worker-1"}
			request.Patch.Labels = &publicops.RevisionTransitionLabelsPatch{Expected: []string{"old"}, Value: []string{"new"}}
			tc.mutate(te, ctx, issue, parent, &request)
			_, err := patcherFor(t, te).TransitionPatch(revisionPatchContext(ctx), request)
			if !errors.Is(err, tc.wantError) {
				t.Fatalf("TransitionPatch error = %v, want %v", err, tc.wantError)
			}
			assertTransitionReceiptCount(t, te, ctx, request.ReceiptID, 0)
			current, err := te.store.GetIssue(ctx, issue.ID)
			if err != nil {
				t.Fatalf("GetIssue after refusal: %v", err)
			}
			if current.Status != types.StatusOpen || current.Assignee != "" || len(current.Labels) != 1 || current.Labels[0] != "old" {
				t.Fatalf("source after refused patch partially changed: %+v", current)
			}
			if !jsonValuesEqual(t, revisionTransitionMetadataValue(t, current.Metadata, "gc_route"), "pending") {
				t.Fatalf("metadata after refused patch = %s, want pending", current.Metadata)
			}
		})
	}
}

func TestRevisionTransitionPatchRefusesWrongParentAndDigest(t *testing.T) {
	skipUnlessEmbeddedDolt(t)
	for index, corrupt := range []func(*publicops.RevisionTransitionPatchRequest){
		func(request *publicops.RevisionTransitionPatchRequest) { request.PriorReceiptID = "missing-parent" },
		func(request *publicops.RevisionTransitionPatchRequest) {
			request.PriorReceiptDigest = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
		},
	} {
		t.Run(fmt.Sprintf("case-%d", index), func(t *testing.T) {
			te := newTestEnv(t, fmt.Sprintf("rpm%d", index))
			ctx := t.Context()
			issue, initial := createRevisionTransitionIssue(t, te, ctx, map[string]interface{}{"gc_route": "pending"})
			parent := createPatchParentReceipt(t, te, ctx, issue, initial, fmt.Sprintf("patch-parent-mismatch-%d", index))
			request := revisionPatchRequest(t, parent, fmt.Sprintf("patch-parent-refused-%d", index))
			corrupt(&request)
			before, err := te.store.GetIssue(ctx, issue.ID)
			if err != nil {
				t.Fatalf("GetIssue before patch: %v", err)
			}
			if _, err := patcherFor(t, te).TransitionPatch(revisionPatchContext(ctx), request); !errors.Is(err, publicops.ErrTransitionReceiptConflict) {
				t.Fatalf("TransitionPatch error = %v, want parent receipt conflict", err)
			}
			assertTransitionReceiptCount(t, te, ctx, request.ReceiptID, 0)
			after, err := te.store.GetIssue(ctx, issue.ID)
			if err != nil || after.RowVersion != before.RowVersion || !jsonValuesEqual(
				t,
				revisionTransitionMetadataValue(t, after.Metadata, "gc_route"),
				revisionTransitionMetadataValue(t, before.Metadata, "gc_route"),
			) {
				t.Fatalf("source changed after parent refusal: before=%+v after=%+v err=%v", before, after, err)
			}
		})
	}
}

func TestRevisionTransitionPatchChainsFromExactPriorPatchReceipt(t *testing.T) {
	skipUnlessEmbeddedDolt(t)
	te := newTestEnv(t, "rpl")
	ctx := t.Context()
	issue, initial := createRevisionTransitionIssue(t, te, ctx, map[string]interface{}{"gc_route": "pending"})
	parent := createPatchParentReceipt(t, te, ctx, issue, initial, "patch-chain-parent")
	firstRequest := revisionPatchRequest(t, parent, "patch-chain-first")
	first := firstRequest.Patch.Metadata[0]
	firstRequest.Patch = publicops.RevisionTransitionIssuePatch{
		Metadata: []publicops.RevisionTransitionMetadataPatch{{
			Key: first.Key, Expected: first.Expected, Value: transitionJSON("ready"),
		}},
	}
	firstResult, err := patcherFor(t, te).TransitionPatch(revisionPatchContext(ctx), firstRequest)
	if err != nil || !firstResult.Applied || firstResult.Receipt == nil {
		t.Fatalf("first patch = %+v, %v", firstResult, err)
	}
	priorDigest, err := publicops.RevisionTransitionPatchReceiptDigest(*firstResult.Receipt)
	if err != nil {
		t.Fatalf("digest first patch receipt: %v", err)
	}
	secondRequest := publicops.RevisionTransitionPatchRequest{
		ProtectedPermit:    "patch-chain-second-permit",
		ReceiptID:          "patch-chain-second",
		IssueID:            issue.ID,
		Scope:              firstRequest.Scope,
		Kind:               firstRequest.Kind,
		Actor:              firstRequest.Actor,
		ExpectedVersion:    firstResult.Receipt.ToVersion,
		PriorReceiptID:     firstResult.Receipt.ReceiptID,
		PriorReceiptDigest: priorDigest,
		Patch: publicops.RevisionTransitionIssuePatch{
			Status: &publicops.RevisionTransitionStringPatch{Expected: "open", Value: "in_progress"},
		},
	}
	secondResult, err := patcherFor(t, te).TransitionPatch(revisionPatchContext(ctx), secondRequest)
	if err != nil || !secondResult.Applied || secondResult.Receipt == nil {
		t.Fatalf("second patch = %+v, %v", secondResult, err)
	}
	if secondResult.Receipt.ExpectedVersion != firstResult.Receipt.ToVersion ||
		secondResult.Receipt.PriorReceiptID != firstResult.Receipt.ReceiptID ||
		secondResult.Receipt.PriorReceiptDigest != priorDigest {
		t.Fatalf("second receipt = %+v, want exact first receipt chain", secondResult.Receipt)
	}
	stored, err := te.store.GetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatalf("GetIssue after chained patch: %v", err)
	}
	if stored.RowVersion != secondResult.Receipt.ToVersion || stored.Status != types.StatusInProgress || !jsonValuesEqual(
		t,
		revisionTransitionMetadataValue(t, stored.Metadata, "gc_route"),
		"ready",
	) {
		t.Fatalf("issue after chained patch = %+v, second receipt = %+v", stored, secondResult.Receipt)
	}
}

func TestRevisionTransitionPatchExactConcurrentRequestsConverge(t *testing.T) {
	skipUnlessEmbeddedDolt(t)
	te := newTestEnv(t, "rpc")
	ctx := t.Context()
	issue, initial := createRevisionTransitionIssue(t, te, ctx, map[string]interface{}{"gc_route": "pending"})
	parent := createPatchParentReceipt(t, te, ctx, issue, initial, "patch-concurrent-parent")
	requestA := revisionPatchRequest(t, parent, "patch-concurrent-id")
	requestA.Patch.Status = &publicops.RevisionTransitionStringPatch{Expected: "open", Value: "in_progress"}
	requestB := requestA
	requestB.ProtectedPermit = "patch-permit-concurrent-b"
	patcher := patcherFor(t, te)
	type outcome struct {
		result publicops.RevisionTransitionPatchResult
		err    error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, 2)
	var wg sync.WaitGroup
	for _, request := range []publicops.RevisionTransitionPatchRequest{requestA, requestB} {
		request := request
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := patcher.TransitionPatch(revisionPatchContext(ctx), request)
			outcomes <- outcome{result: result, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(outcomes)
	var toVersion int64
	for got := range outcomes {
		if got.err != nil {
			t.Fatalf("concurrent TransitionPatch: %v", got.err)
		}
		if !got.result.Applied || got.result.Receipt == nil {
			t.Fatalf("concurrent result = %+v, want receipt", got.result)
		}
		if toVersion != 0 && got.result.Receipt.ToVersion != toVersion {
			t.Fatalf("concurrent receipts disagree: %d vs %d", toVersion, got.result.Receipt.ToVersion)
		}
		toVersion = got.result.Receipt.ToVersion
	}
	assertTransitionReceiptCount(t, te, ctx, requestA.ReceiptID, 1)
}

func jsonValuesEqual(t *testing.T, value any, want any) bool {
	t.Helper()
	actualJSON, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal actual value: %v", err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal wanted value: %v", err)
	}
	var actual, expected any
	if err := json.Unmarshal(actualJSON, &actual); err != nil {
		t.Fatalf("decode actual value: %v", err)
	}
	if err := json.Unmarshal(wantJSON, &expected); err != nil {
		t.Fatalf("decode wanted value: %v", err)
	}
	return reflect.DeepEqual(actual, expected)
}

func revisionTransitionMetadataValue(t *testing.T, metadata json.RawMessage, key string) json.RawMessage {
	t.Helper()
	if len(metadata) == 0 {
		t.Fatalf("metadata is empty; want key %q", key)
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(metadata, &values); err != nil {
		t.Fatalf("decode issue metadata %s: %v", metadata, err)
	}
	value, ok := values[key]
	if !ok {
		t.Fatalf("metadata %s is missing key %q", metadata, key)
	}
	return value
}
