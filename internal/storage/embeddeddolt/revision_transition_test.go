//go:build cgo

package embeddeddolt_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/rhar1511/beads/internal/storage"
	"github.com/rhar1511/beads/internal/types"
	publicops "github.com/rhar1511/beads/issueops"
)

const revisionTransitionTestKey = "gc_test_transition"

func createRevisionTransitionIssue(t *testing.T, te *testEnv, ctx context.Context, metadata map[string]interface{}) (*types.Issue, int64) {
	t.Helper()
	var metadataJSON json.RawMessage
	if metadata != nil {
		encoded, err := json.Marshal(metadata)
		if err != nil {
			t.Fatalf("marshal fixture metadata: %v", err)
		}
		metadataJSON = encoded
	}
	issue := &types.Issue{
		Title:     "revision transition fixture",
		Status:    types.StatusOpen,
		Priority:  2,
		IssueType: types.TypeTask,
		Metadata:  metadataJSON,
	}
	if err := te.store.CreateIssue(ctx, issue, "revision-test"); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	var revision int64
	te.queryScalar(t, ctx, "SELECT row_lock FROM issues WHERE id = ?", []any{issue.ID}, &revision)
	if revision == 0 {
		t.Fatal("created issue row_lock = 0, want nonzero revision")
	}
	return issue, revision
}

func transitionJSON(value string) *json.RawMessage {
	encoded, _ := json.Marshal(value)
	raw := json.RawMessage(encoded)
	return &raw
}

func transitionRequest(receiptID, issueID string, revision int64, expected, value *json.RawMessage) publicops.RevisionTransitionRequest {
	return publicops.RevisionTransitionRequest{
		ReceiptID:       receiptID,
		IssueID:         issueID,
		Scope:           "test-scope",
		Kind:            "ensure",
		Actor:           "revision-test",
		ExpectedVersion: revision,
		Key:             revisionTransitionTestKey,
		Expected:        expected,
		Value:           value,
		Payload:         json.RawMessage(`{"ticket":"test"}`),
	}
}

func assertTransitionReceiptCount(t *testing.T, te *testEnv, ctx context.Context, receiptID string, want int) {
	t.Helper()
	var count int
	te.queryScalar(t, ctx, "SELECT COUNT(*) FROM issue_transition_receipts WHERE receipt_id = ?", []any{receiptID}, &count)
	if count != want {
		t.Fatalf("receipt count for %s = %d, want %d", receiptID, count, want)
	}
}

func TestRevisionTransitionAppliesAndReceiptCapturesWrittenRevision(t *testing.T) {
	skipUnlessEmbeddedDolt(t)
	te := newTestEnv(t, "rta")
	ctx := t.Context()
	issue, before := createRevisionTransitionIssue(t, te, ctx, nil)
	role, err := te.store.RevisionTransition()
	if err != nil {
		t.Fatalf("RevisionTransition: %v", err)
	}

	req := transitionRequest("revision-apply", issue.ID, before, nil, transitionJSON("ready"))
	result, err := role.TransitionMetadata(ctx, req)
	if err != nil {
		t.Fatalf("TransitionMetadata: %v", err)
	}
	if !result.Applied || result.Replayed || result.Receipt == nil {
		t.Fatalf("TransitionMetadata result = %+v, want newly applied receipt", result)
	}
	if result.Receipt.ExpectedVersion != before || result.Receipt.ToVersion == 0 || result.Receipt.ToVersion == before {
		t.Fatalf("receipt revisions = %d -> %d, want nonzero and different", result.Receipt.ExpectedVersion, result.Receipt.ToVersion)
	}
	var rowLock int64
	te.queryScalar(t, ctx, "SELECT row_lock FROM issues WHERE id = ?", []any{issue.ID}, &rowLock)
	if rowLock != result.Receipt.ToVersion {
		t.Fatalf("persisted row_lock = %d, receipt to_revision = %d", rowLock, result.Receipt.ToVersion)
	}
	var marker sql.NullString
	te.queryScalar(t, ctx, "SELECT JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.gc_test_transition')) FROM issues WHERE id = ?", []any{issue.ID}, &marker)
	if !marker.Valid || marker.String != "ready" {
		t.Fatalf("transition marker = %v, want ready", marker)
	}
	stored, err := role.GetReceipt(ctx, req.ReceiptID)
	if err != nil {
		t.Fatalf("GetReceipt: %v", err)
	}
	if stored == nil || stored.ToVersion != result.Receipt.ToVersion || stored.Value == nil || string(*stored.Value) != `"ready"` {
		t.Fatalf("stored receipt = %+v, want actual transition receipt", stored)
	}
}

func TestRevisionTransitionStaleRevisionWritesNothing(t *testing.T) {
	skipUnlessEmbeddedDolt(t)
	te := newTestEnv(t, "rts")
	ctx := t.Context()
	issue, stale := createRevisionTransitionIssue(t, te, ctx, nil)
	if err := te.store.UpdateIssue(ctx, issue.ID, map[string]interface{}{"title": "changed after read"}, "revision-test"); err != nil {
		t.Fatalf("UpdateIssue: %v", err)
	}
	var current int64
	te.queryScalar(t, ctx, "SELECT row_lock FROM issues WHERE id = ?", []any{issue.ID}, &current)
	if current == stale {
		t.Fatal("setup update did not change row_lock")
	}
	beforeAttempt := current

	role, err := te.store.RevisionTransition()
	if err != nil {
		t.Fatalf("RevisionTransition: %v", err)
	}
	req := transitionRequest("revision-stale", issue.ID, stale, nil, transitionJSON("ready"))
	if _, err := role.TransitionMetadata(ctx, req); !errors.Is(err, storage.ErrVersionMismatch) {
		t.Fatalf("TransitionMetadata error = %v, want ErrVersionMismatch", err)
	}
	assertTransitionReceiptCount(t, te, ctx, req.ReceiptID, 0)
	var marker sql.NullString
	te.queryScalar(t, ctx, "SELECT JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.gc_test_transition')) FROM issues WHERE id = ?", []any{issue.ID}, &marker)
	if marker.Valid {
		t.Fatalf("marker after stale refusal = %q, want absent", marker.String)
	}
	te.queryScalar(t, ctx, "SELECT row_lock FROM issues WHERE id = ?", []any{issue.ID}, &current)
	if current != beforeAttempt {
		t.Fatalf("row_lock after stale refusal = %d, want unchanged value %d", current, beforeAttempt)
	}
}

func TestRevisionTransitionMarkerMismatchWritesNothing(t *testing.T) {
	skipUnlessEmbeddedDolt(t)
	te := newTestEnv(t, "rtm")
	ctx := t.Context()
	issue, before := createRevisionTransitionIssue(t, te, ctx, map[string]interface{}{revisionTransitionTestKey: "actual"})
	role, err := te.store.RevisionTransition()
	if err != nil {
		t.Fatalf("RevisionTransition: %v", err)
	}
	req := transitionRequest("revision-marker-mismatch", issue.ID, before, transitionJSON("expected"), transitionJSON("next"))
	result, err := role.TransitionMetadata(ctx, req)
	if err != nil {
		t.Fatalf("TransitionMetadata: %v", err)
	}
	if result.Applied || result.Replayed || result.Receipt != nil || result.Current == nil || string(*result.Current) != `"actual"` {
		t.Fatalf("TransitionMetadata result = %+v, want marker refusal with actual current value", result)
	}
	assertTransitionReceiptCount(t, te, ctx, req.ReceiptID, 0)
	var current int64
	te.queryScalar(t, ctx, "SELECT row_lock FROM issues WHERE id = ?", []any{issue.ID}, &current)
	if current != before {
		t.Fatalf("row_lock after marker refusal = %d, want unchanged %d", current, before)
	}
}

func TestRevisionTransitionAbsentAndNullMarkersAreDistinct(t *testing.T) {
	skipUnlessEmbeddedDolt(t)
	te := newTestEnv(t, "rtn")
	ctx := t.Context()
	issue, revision := createRevisionTransitionIssue(t, te, ctx, map[string]interface{}{revisionTransitionTestKey: nil})
	role, err := te.store.RevisionTransition()
	if err != nil {
		t.Fatalf("RevisionTransition: %v", err)
	}

	absentReq := transitionRequest("revision-null-as-absent", issue.ID, revision, nil, transitionJSON("ready"))
	absentResult, err := role.TransitionMetadata(ctx, absentReq)
	if err != nil {
		t.Fatalf("TransitionMetadata with absent expected marker: %v", err)
	}
	if absentResult.Applied || absentResult.Current == nil || string(*absentResult.Current) != "null" {
		t.Fatalf("absent-marker result = %+v, want refusal against present null", absentResult)
	}
	assertTransitionReceiptCount(t, te, ctx, absentReq.ReceiptID, 0)

	var rowLock int64
	te.queryScalar(t, ctx, "SELECT row_lock FROM issues WHERE id = ?", []any{issue.ID}, &rowLock)
	nullValue := json.RawMessage("null")
	nullReq := transitionRequest("revision-null-as-value", issue.ID, rowLock, &nullValue, transitionJSON("ready"))
	nullResult, err := role.TransitionMetadata(ctx, nullReq)
	if err != nil {
		t.Fatalf("TransitionMetadata with explicit null expected marker: %v", err)
	}
	if !nullResult.Applied || nullResult.Receipt == nil {
		t.Fatalf("explicit-null result = %+v, want applied transition", nullResult)
	}
}

func TestRevisionTransitionExactRetryReplaysAfterLaterSourceChange(t *testing.T) {
	skipUnlessEmbeddedDolt(t)
	te := newTestEnv(t, "rtr")
	ctx := t.Context()
	issue, revision := createRevisionTransitionIssue(t, te, ctx, nil)
	role, err := te.store.RevisionTransition()
	if err != nil {
		t.Fatalf("RevisionTransition: %v", err)
	}
	req := transitionRequest("revision-replay", issue.ID, revision, nil, transitionJSON("ready"))
	first, err := role.TransitionMetadata(ctx, req)
	if err != nil || !first.Applied || first.Receipt == nil {
		t.Fatalf("first TransitionMetadata = %+v, %v", first, err)
	}
	if err := te.store.UpdateIssue(ctx, issue.ID, map[string]interface{}{"title": "later change"}, "revision-test"); err != nil {
		t.Fatalf("later UpdateIssue: %v", err)
	}
	var afterLaterChange int64
	te.queryScalar(t, ctx, "SELECT row_lock FROM issues WHERE id = ?", []any{issue.ID}, &afterLaterChange)
	if afterLaterChange == first.Receipt.ToVersion {
		t.Fatal("later source mutation did not advance row_lock")
	}

	second, err := role.TransitionMetadata(ctx, req)
	if err != nil {
		t.Fatalf("retry TransitionMetadata: %v", err)
	}
	if !second.Applied || !second.Replayed || second.Receipt == nil || second.Receipt.ToVersion != first.Receipt.ToVersion {
		t.Fatalf("retry result = %+v, want replay of original receipt", second)
	}
	var afterReplay int64
	te.queryScalar(t, ctx, "SELECT row_lock FROM issues WHERE id = ?", []any{issue.ID}, &afterReplay)
	if afterReplay != afterLaterChange {
		t.Fatalf("row_lock after replay = %d, want unchanged %d", afterReplay, afterLaterChange)
	}
}

func TestRevisionTransitionReceiptIDBindsEveryRequestField(t *testing.T) {
	skipUnlessEmbeddedDolt(t)
	te := newTestEnv(t, "rtc")
	ctx := t.Context()
	issue, revision := createRevisionTransitionIssue(t, te, ctx, nil)
	other, _ := createRevisionTransitionIssue(t, te, ctx, nil)
	role, err := te.store.RevisionTransition()
	if err != nil {
		t.Fatalf("RevisionTransition: %v", err)
	}
	base := transitionRequest("revision-bound", issue.ID, revision, nil, transitionJSON("ready"))
	if result, err := role.TransitionMetadata(ctx, base); err != nil || !result.Applied {
		t.Fatalf("initial transition = %+v, %v", result, err)
	}
	changedExpected := transitionJSON("another marker")
	tests := []struct {
		name   string
		mutate func(*publicops.RevisionTransitionRequest)
	}{
		{name: "issue id", mutate: func(r *publicops.RevisionTransitionRequest) { r.IssueID = other.ID }},
		{name: "scope", mutate: func(r *publicops.RevisionTransitionRequest) { r.Scope = "other-scope" }},
		{name: "kind", mutate: func(r *publicops.RevisionTransitionRequest) { r.Kind = "release" }},
		{name: "actor", mutate: func(r *publicops.RevisionTransitionRequest) { r.Actor = "other-actor" }},
		{name: "expected revision", mutate: func(r *publicops.RevisionTransitionRequest) { r.ExpectedVersion = revision + 1 }},
		{name: "metadata key", mutate: func(r *publicops.RevisionTransitionRequest) { r.Key = "gc_other_transition" }},
		{name: "expected value", mutate: func(r *publicops.RevisionTransitionRequest) { r.Expected = changedExpected }},
		{name: "next value", mutate: func(r *publicops.RevisionTransitionRequest) { r.Value = transitionJSON("another value") }},
		{name: "payload", mutate: func(r *publicops.RevisionTransitionRequest) { r.Payload = json.RawMessage(`{"ticket":"other"}`) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := base
			tt.mutate(&req)
			if _, err := role.TransitionMetadata(ctx, req); !errors.Is(err, publicops.ErrTransitionReceiptConflict) {
				t.Fatalf("TransitionMetadata error = %v, want ErrTransitionReceiptConflict", err)
			}
		})
	}
}

func TestRevisionTransitionReceiptSurvivesIssueDeletion(t *testing.T) {
	skipUnlessEmbeddedDolt(t)
	te := newTestEnv(t, "rtd")
	ctx := t.Context()
	issue, revision := createRevisionTransitionIssue(t, te, ctx, nil)
	role, err := te.store.RevisionTransition()
	if err != nil {
		t.Fatalf("RevisionTransition: %v", err)
	}
	req := transitionRequest("revision-survives-delete", issue.ID, revision, nil, transitionJSON("ready"))
	if result, err := role.TransitionMetadata(ctx, req); err != nil || !result.Applied {
		t.Fatalf("TransitionMetadata = %+v, %v", result, err)
	}
	if err := te.store.DeleteIssue(ctx, issue.ID); err != nil {
		t.Fatalf("DeleteIssue: %v", err)
	}
	receipt, err := role.GetReceipt(ctx, req.ReceiptID)
	if err != nil {
		t.Fatalf("GetReceipt after issue deletion: %v", err)
	}
	if receipt == nil || receipt.IssueID != issue.ID {
		t.Fatalf("receipt after issue deletion = %+v, want durable receipt for %s", receipt, issue.ID)
	}
	replay, err := role.TransitionMetadata(ctx, req)
	if err != nil {
		t.Fatalf("TransitionMetadata retry after issue deletion: %v", err)
	}
	if !replay.Applied || !replay.Replayed || replay.Receipt == nil || replay.Receipt.ReceiptID != req.ReceiptID {
		t.Fatalf("TransitionMetadata retry after issue deletion = %+v, want exact replay", replay)
	}
}

func TestRevisionTransitionConcurrentExactRequestHasOneWriter(t *testing.T) {
	skipUnlessEmbeddedDolt(t)
	te := newTestEnv(t, "rtq")
	ctx := t.Context()
	issue, revision := createRevisionTransitionIssue(t, te, ctx, nil)
	role, err := te.store.RevisionTransition()
	if err != nil {
		t.Fatalf("RevisionTransition: %v", err)
	}
	req := transitionRequest("revision-concurrent", issue.ID, revision, nil, transitionJSON("ready"))
	start := make(chan struct{})
	type outcome struct {
		result publicops.RevisionTransitionResult
		err    error
	}
	outcomes := make(chan outcome, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := role.TransitionMetadata(ctx, req)
			outcomes <- outcome{result: result, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(outcomes)
	var applied, replayed int
	for outcome := range outcomes {
		if outcome.err != nil {
			t.Fatalf("concurrent TransitionMetadata: %v", outcome.err)
		}
		if outcome.result.Applied && !outcome.result.Replayed {
			applied++
		}
		if outcome.result.Applied && outcome.result.Replayed {
			replayed++
		}
	}
	if applied != 1 || replayed != 1 {
		t.Fatalf("concurrent outcomes = %d applied, %d replayed, want one each (%s)", applied, replayed, fmt.Sprint(req.ReceiptID))
	}
}

func TestRevisionTransitionRefusesWispBackedSourceBeforeWriting(t *testing.T) {
	skipUnlessEmbeddedDolt(t)
	te := newTestEnv(t, "rts")
	ctx := t.Context()

	t.Run("wisp source", func(t *testing.T) {
		wisp := &types.Issue{
			Title:     "wisp source",
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeTask,
			Ephemeral: true,
		}
		if err := te.store.CreateIssue(ctx, wisp, "revision-test"); err != nil {
			t.Fatalf("CreateIssue(wisp): %v", err)
		}
		assertRevisionTransitionWispRefusal(t, te, ctx, wisp.ID, "wisp-source")
	})

	t.Run("durable source with wisp target", func(t *testing.T) {
		source := &types.Issue{Title: "durable source", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}
		target := &types.Issue{Title: "wisp target", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask, Ephemeral: true}
		if err := te.store.CreateIssue(ctx, source, "revision-test"); err != nil {
			t.Fatalf("CreateIssue(source): %v", err)
		}
		if err := te.store.CreateIssue(ctx, target, "revision-test"); err != nil {
			t.Fatalf("CreateIssue(target): %v", err)
		}
		if err := te.store.AddDependency(ctx, &types.Dependency{
			IssueID: source.ID, DependsOnID: target.ID, Type: types.DepRelated,
		}, "revision-test"); err != nil {
			t.Fatalf("AddDependency(source -> wisp): %v", err)
		}
		assertRevisionTransitionWispRefusal(t, te, ctx, source.ID, "wisp-target")
	})
}

func assertRevisionTransitionWispRefusal(t *testing.T, te *testEnv, ctx context.Context, issueID, suffix string) {
	t.Helper()
	var rowLock int64
	table := "issues"
	var wisp int
	te.queryScalar(t, ctx, "SELECT COUNT(*) FROM wisps WHERE id = ?", []any{issueID}, &wisp)
	if wisp != 0 {
		table = "wisps"
	}
	te.queryScalar(t, ctx, "SELECT row_lock FROM "+table+" WHERE id = ?", []any{issueID}, &rowLock)
	if rowLock == 0 {
		t.Fatal("fixture row_lock = 0, want a nonzero revision token")
	}

	transition, err := te.store.RevisionTransition()
	if err != nil {
		t.Fatalf("RevisionTransition: %v", err)
	}
	value := json.RawMessage(`"held"`)
	receiptID := "revision-wisp-" + suffix
	_, err = transition.TransitionMetadata(ctx, publicops.RevisionTransitionRequest{
		ReceiptID:       receiptID,
		IssueID:         issueID,
		Scope:           "test-scope",
		Kind:            "ensure",
		Actor:           "revision-test",
		ExpectedVersion: rowLock,
		Key:             "gc_test_transition",
		Value:           &value,
	})
	var unsupported *storage.ErrUnsupported
	if !errors.As(err, &unsupported) {
		t.Fatalf("TransitionMetadata error = %v, want ErrUnsupported", err)
	}
	if unsupported.Op != "RevisionTransition" {
		t.Fatalf("ErrUnsupported.Op = %q, want RevisionTransition", unsupported.Op)
	}

	var receipts int
	te.queryScalar(t, ctx, "SELECT COUNT(*) FROM issue_transition_receipts WHERE receipt_id = ?", []any{receiptID}, &receipts)
	if receipts != 0 {
		t.Fatalf("transition receipt count = %d, want 0", receipts)
	}
	var marker sql.NullString
	te.queryScalar(t, ctx, "SELECT JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.gc_test_transition')) FROM "+table+" WHERE id = ?", []any{issueID}, &marker)
	if marker.Valid {
		t.Fatalf("metadata marker = %q, want absent after refusal", marker.String)
	}
	var current int64
	te.queryScalar(t, ctx, "SELECT row_lock FROM "+table+" WHERE id = ?", []any{issueID}, &current)
	if current != rowLock {
		t.Fatalf("row_lock after refusal = %d, want unchanged %d", current, rowLock)
	}
}
