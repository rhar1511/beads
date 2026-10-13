//go:build cgo

package embeddeddolt_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/embeddeddolt"
	"github.com/steveyegge/beads/internal/types"
	publicops "github.com/steveyegge/beads/issueops"
)

// These aggregate-revision cases preserve the source-completeness design and
// behavioral intent of gastownhall/beads#4682 and #4697 (Julian Knutsen),
// retargeted to current main's existing row_lock / RowVersion token.
func TestAggregateRevisionChangesForEveryVisibleAuxiliaryMutation(t *testing.T) {
	skipUnlessEmbeddedDolt(t)
	te := newTestEnv(t, "aggregate_revision")
	ctx := t.Context()
	ops, err := embeddeddolt.NewIssueOperations(te.store)
	if err != nil {
		t.Fatal(err)
	}
	commenter, err := embeddeddolt.NewCommenter(te.store)
	if err != nil {
		t.Fatal(err)
	}
	dependencies, err := embeddeddolt.NewDependencyEditor(te.store)
	if err != nil {
		t.Fatal(err)
	}

	create := func(t *testing.T, id string, wisp bool) *types.Issue {
		t.Helper()
		result, err := ops.Create(ctx, publicops.CreateRequest{
			Actor:         "seed",
			ForceIDPrefix: true,
			Issue: &types.Issue{ID: id, Title: id, Status: types.StatusOpen,
				Priority: 2, IssueType: types.TypeTask, Ephemeral: wisp},
		})
		if err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
		if result.Issue.RowVersion == 0 {
			t.Fatalf("create %s returned zero RowVersion", id)
		}
		return result.Issue
	}
	assertChanged := func(t *testing.T, id string, before int64) int64 {
		t.Helper()
		after, err := te.store.GetIssue(ctx, id)
		if err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if after.RowVersion == 0 || after.RowVersion == before {
			t.Fatalf("%s RowVersion = %d after mutation, want non-zero and different from %d", id, after.RowVersion, before)
		}
		return after.RowVersion
	}

	for _, wisp := range []bool{false, true} {
		plane := "issue"
		if wisp {
			plane = "wisp"
		}
		t.Run(plane, func(t *testing.T) {
			label := create(t, "revision-"+plane+"-label", wisp)
			if err := te.store.AddLabel(ctx, label.ID, "added", "writer"); err != nil {
				t.Fatal(err)
			}
			version := assertChanged(t, label.ID, label.RowVersion)
			if err := te.store.RemoveLabel(ctx, label.ID, "added", "writer"); err != nil {
				t.Fatal(err)
			}
			assertChanged(t, label.ID, version)

			comment := create(t, "revision-"+plane+"-comment", wisp)
			if _, err := commenter.AddComment(ctx, publicops.AddCommentRequest{Author: "writer", IssueID: comment.ID, Text: "visible comment"}); err != nil {
				t.Fatal(err)
			}
			assertChanged(t, comment.ID, comment.RowVersion)

			target := create(t, "revision-"+plane+"-target", false)
			dependency := create(t, "revision-"+plane+"-dependency", wisp)
			if _, err := dependencies.AddDependencies(ctx, publicops.AddDependenciesRequest{Actor: "writer", Edges: []publicops.DependencyEdge{{IssueID: dependency.ID, DependsOnID: target.ID, Type: publicops.DepRelated}}}); err != nil {
				t.Fatal(err)
			}
			version = assertChanged(t, dependency.ID, dependency.RowVersion)
			if _, err := dependencies.RemoveDependency(ctx, publicops.RemoveDependencyRequest{Actor: "writer", IssueID: dependency.ID, DependsOnID: target.ID}); err != nil {
				t.Fatal(err)
			}
			assertChanged(t, dependency.ID, version)

			metadata := create(t, "revision-"+plane+"-metadata", wisp)
			updated, err := ops.Update(ctx, publicops.UpdateRequest{Actor: "writer", IssueID: metadata.ID, Patch: publicops.IssuePatch{Metadata: publicops.MetadataPatch{Set: map[string]json.RawMessage{"decision": json.RawMessage(`"yes"`)}}}})
			if err != nil || !updated.Changed {
				t.Fatalf("metadata update: %#v, %v", updated, err)
			}
			assertChanged(t, metadata.ID, metadata.RowVersion)

			parent := create(t, "revision-"+plane+"-parent", false)
			child := create(t, "revision-"+plane+"-child", wisp)
			updated, err = ops.Update(ctx, publicops.UpdateRequest{Actor: "writer", IssueID: child.ID, Patch: publicops.IssuePatch{ParentID: publicops.Field[string]{Set: true, Value: parent.ID}}})
			if err != nil || !updated.Changed {
				t.Fatalf("parent update: %#v, %v", updated, err)
			}
			assertChanged(t, child.ID, child.RowVersion)
		})
	}
}

func TestAggregateRevisionTargetRenamesInvalidateInboundSources(t *testing.T) {
	skipUnlessEmbeddedDolt(t)
	te := newTestEnv(t, "aggregate_revision_rename")
	ctx := t.Context()

	create := func(id string, wisp bool) *types.Issue {
		t.Helper()
		issue := &types.Issue{ID: id, Title: id, Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask, Ephemeral: wisp}
		if err := te.store.CreateIssue(ctx, issue, "seed"); err != nil {
			t.Fatalf("CreateIssue(%s): %v", id, err)
		}
		return issue
	}
	rename := func(oldID, newID string) {
		t.Helper()
		issue, err := te.store.GetIssue(ctx, oldID)
		if err != nil {
			t.Fatalf("GetIssue(%s): %v", oldID, err)
		}
		issue.ID = newID
		if err := te.store.UpdateIssueID(ctx, oldID, newID, issue, "writer"); err != nil {
			t.Fatalf("UpdateIssueID(%s -> %s): %v", oldID, newID, err)
		}
		if err := te.store.Commit(ctx, "test: rename dependency target"); err != nil {
			t.Fatalf("commit rename %s -> %s: %v", oldID, newID, err)
		}
	}
	assertSourceChanged := func(sourceID string, before int64) {
		t.Helper()
		after, err := te.store.GetIssue(ctx, sourceID)
		if err != nil {
			t.Fatalf("GetIssue(%s): %v", sourceID, err)
		}
		if after.RowVersion == 0 || after.RowVersion == before {
			t.Fatalf("source %s RowVersion = %d after target rename, want nonzero and different from %d", sourceID, after.RowVersion, before)
		}
		var committedRevision int64
		te.queryScalar(t, ctx, "SELECT row_lock FROM issues AS OF 'HEAD' WHERE id = ?", []any{sourceID}, &committedRevision)
		if committedRevision != after.RowVersion {
			t.Fatalf("source %s committed row_lock = %d, returned RowVersion = %d", sourceID, committedRevision, after.RowVersion)
		}
	}

	issueSource := create("revision-rename-issue-source", false)
	issueTarget := create("revision-rename-issue-target", false)
	if err := te.store.AddDependency(ctx, &types.Dependency{IssueID: issueSource.ID, DependsOnID: issueTarget.ID, Type: types.DepRelated}, "writer"); err != nil {
		t.Fatalf("AddDependency issue target: %v", err)
	}
	issueSourceAfterAdd, err := te.store.GetIssue(ctx, issueSource.ID)
	if err != nil {
		t.Fatalf("GetIssue(%s): %v", issueSource.ID, err)
	}
	rename(issueTarget.ID, "revision-rename-issue-target-new")
	assertSourceChanged(issueSource.ID, issueSourceAfterAdd.RowVersion)

	wispSource := create("revision-rename-wisp-source", false)
	wispTarget := create("revision-rename-wisp-target", true)
	if err := te.store.AddDependency(ctx, &types.Dependency{IssueID: wispSource.ID, DependsOnID: wispTarget.ID, Type: types.DepRelated}, "writer"); err != nil {
		t.Fatalf("AddDependency wisp target: %v", err)
	}
	wispSourceAfterAdd, err := te.store.GetIssue(ctx, wispSource.ID)
	if err != nil {
		t.Fatalf("GetIssue(%s): %v", wispSource.ID, err)
	}
	rename(wispTarget.ID, "revision-rename-wisp-target-new")
	assertSourceChanged(wispSource.ID, wispSourceAfterAdd.RowVersion)
}

func TestAggregateRevisionPromotionFencesNewDurableEdgesAndRetargetedSources(t *testing.T) {
	skipUnlessEmbeddedDolt(t)
	te := newTestEnv(t, "aggregate_revision_promote")
	ctx := t.Context()
	create := func(id string, wisp bool) *types.Issue {
		t.Helper()
		issue := &types.Issue{ID: id, Title: id, Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask, Ephemeral: wisp}
		if err := te.store.CreateIssue(ctx, issue, "seed"); err != nil {
			t.Fatalf("CreateIssue(%s): %v", id, err)
		}
		return issue
	}

	toPromote := create("revision-promote-target", true)
	outgoingTarget := create("revision-promote-outgoing-target", false)
	inboundSource := create("revision-promote-inbound-source", false)
	if err := te.store.AddDependency(ctx, &types.Dependency{IssueID: toPromote.ID, DependsOnID: outgoingTarget.ID, Type: types.DepRelated}, "writer"); err != nil {
		t.Fatalf("AddDependency wisp outgoing edge: %v", err)
	}
	if err := te.store.AddDependency(ctx, &types.Dependency{IssueID: inboundSource.ID, DependsOnID: toPromote.ID, Type: types.DepRelated}, "writer"); err != nil {
		t.Fatalf("AddDependency durable inbound edge: %v", err)
	}
	wispBefore, err := te.store.GetIssue(ctx, toPromote.ID)
	if err != nil {
		t.Fatalf("GetIssue(%s): %v", toPromote.ID, err)
	}
	inboundBefore, err := te.store.GetIssue(ctx, inboundSource.ID)
	if err != nil {
		t.Fatalf("GetIssue(%s): %v", inboundSource.ID, err)
	}
	if wispBefore.RowVersion == 0 || inboundBefore.RowVersion == 0 {
		t.Fatal("promotion fixture has zero RowVersion")
	}

	if err := te.store.PromoteFromEphemeral(ctx, toPromote.ID, "writer"); err != nil {
		t.Fatalf("PromoteFromEphemeral: %v", err)
	}
	if err := te.store.Commit(ctx, "test: promote aggregate revision"); err != nil {
		t.Fatalf("commit promotion: %v", err)
	}
	promoted, err := te.store.GetIssue(ctx, toPromote.ID)
	if err != nil {
		t.Fatalf("GetIssue promoted issue: %v", err)
	}
	if promoted.RowVersion == 0 || promoted.RowVersion == wispBefore.RowVersion {
		t.Fatalf("promoted RowVersion = %d, want nonzero token different from wisp token %d", promoted.RowVersion, wispBefore.RowVersion)
	}
	var committedRevision int64
	te.queryScalar(t, ctx, "SELECT row_lock FROM issues AS OF 'HEAD' WHERE id = ?", []any{toPromote.ID}, &committedRevision)
	if committedRevision != promoted.RowVersion {
		t.Fatalf("promoted committed row_lock = %d, returned RowVersion = %d", committedRevision, promoted.RowVersion)
	}
	var promotedOutgoingEdges int
	te.queryScalar(t, ctx, "SELECT COUNT(*) FROM dependencies WHERE issue_id = ? AND depends_on_issue_id = ?", []any{toPromote.ID, outgoingTarget.ID}, &promotedOutgoingEdges)
	if promotedOutgoingEdges != 1 {
		t.Fatalf("promoted durable outgoing edge count = %d, want 1", promotedOutgoingEdges)
	}
	inboundAfter, err := te.store.GetIssue(ctx, inboundSource.ID)
	if err != nil {
		t.Fatalf("GetIssue(%s): %v", inboundSource.ID, err)
	}
	if inboundAfter.RowVersion == 0 || inboundAfter.RowVersion == inboundBefore.RowVersion {
		t.Fatalf("inbound source RowVersion = %d after target promotion, want nonzero and different from %d", inboundAfter.RowVersion, inboundBefore.RowVersion)
	}
	var inboundCommittedRevision int64
	te.queryScalar(t, ctx, "SELECT row_lock FROM issues AS OF 'HEAD' WHERE id = ?", []any{inboundSource.ID}, &inboundCommittedRevision)
	if inboundCommittedRevision != inboundAfter.RowVersion {
		t.Fatalf("inbound source committed row_lock = %d, returned RowVersion = %d", inboundCommittedRevision, inboundAfter.RowVersion)
	}
	var inboundIssueTarget int
	te.queryScalar(t, ctx, "SELECT COUNT(*) FROM dependencies WHERE issue_id = ? AND depends_on_issue_id = ?", []any{inboundSource.ID, toPromote.ID}, &inboundIssueTarget)
	if inboundIssueTarget != 1 {
		t.Fatalf("retargeted durable dependency count = %d, want 1", inboundIssueTarget)
	}
}

func TestAggregateRevisionAuxiliaryMutationInvalidatesGuard(t *testing.T) {
	skipUnlessEmbeddedDolt(t)
	te := newTestEnv(t, "aggregate_revision_stale")
	ctx := t.Context()
	ops, err := embeddeddolt.NewIssueOperations(te.store)
	if err != nil {
		t.Fatal(err)
	}
	dependencies, err := embeddeddolt.NewDependencyEditor(te.store)
	if err != nil {
		t.Fatal(err)
	}
	create := func(t *testing.T, id string) *types.Issue {
		t.Helper()
		result, err := ops.Create(ctx, publicops.CreateRequest{Actor: "seed", ForceIDPrefix: true, Issue: &types.Issue{ID: id, Title: id, Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}})
		if err != nil {
			t.Fatal(err)
		}
		return result.Issue
	}

	target := create(t, "stale-target")
	parent := create(t, "stale-parent-target")
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, *types.Issue)
	}{
		{"label", func(t *testing.T, issue *types.Issue) {
			t.Helper()
			mustNoError(t, te.store.AddLabel(ctx, issue.ID, "new", "writer"))
		}},
		{"metadata", func(t *testing.T, issue *types.Issue) {
			t.Helper()
			_, err := ops.Update(ctx, publicops.UpdateRequest{Actor: "writer", IssueID: issue.ID, Patch: publicops.IssuePatch{Metadata: publicops.MetadataPatch{Set: map[string]json.RawMessage{"x": json.RawMessage(`1`)}}}})
			mustNoError(t, err)
		}},
		{"parent", func(t *testing.T, issue *types.Issue) {
			t.Helper()
			_, err := ops.Update(ctx, publicops.UpdateRequest{Actor: "writer", IssueID: issue.ID, Patch: publicops.IssuePatch{ParentID: publicops.Field[string]{Set: true, Value: parent.ID}}})
			mustNoError(t, err)
		}},
		{"dependency", func(t *testing.T, issue *types.Issue) {
			t.Helper()
			_, err := dependencies.AddDependencies(ctx, publicops.AddDependenciesRequest{Actor: "writer", Edges: []publicops.DependencyEdge{{IssueID: issue.ID, DependsOnID: target.ID, Type: publicops.DepRelated}}})
			mustNoError(t, err)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issue := create(t, "stale-"+tc.name)
			stale := issue.RowVersion
			tc.mutate(t, issue)
			_, err := ops.Update(ctx, publicops.UpdateRequest{Actor: "writer", IssueID: issue.ID, ExpectedVersion: &stale, Patch: publicops.IssuePatch{Title: publicops.Field[string]{Set: true, Value: "must not land"}}})
			if !errors.Is(err, storage.ErrVersionMismatch) {
				t.Fatalf("guard after %s mutation = %v, want ErrVersionMismatch", tc.name, err)
			}
		})
	}
}

func TestAggregateRevisionCreateWritesBackAndNoOpsStayStable(t *testing.T) {
	skipUnlessEmbeddedDolt(t)
	te := newTestEnv(t, "aggregate_revision_noop")
	ctx := t.Context()
	issue := &types.Issue{ID: "revision-create-readback", Title: "create", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}
	if err := te.store.CreateIssue(ctx, issue, "writer"); err != nil {
		t.Fatal(err)
	}
	if issue.RowVersion == 0 {
		t.Fatal("CreateIssue left caller's RowVersion at zero")
	}
	if err := te.store.AddLabel(ctx, issue.ID, "same", "writer"); err != nil {
		t.Fatal(err)
	}
	stored, err := te.store.GetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatal(err)
	}
	eventsBeforeNoOps, err := te.store.GetEvents(ctx, issue.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := te.store.AddLabel(ctx, issue.ID, "same", "writer"); err != nil {
		t.Fatal(err)
	}
	if err := te.store.RemoveLabel(ctx, issue.ID, "missing", "writer"); err != nil {
		t.Fatal(err)
	}
	after, err := te.store.GetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.RowVersion != stored.RowVersion {
		t.Fatalf("idempotent label operations changed RowVersion %d -> %d", stored.RowVersion, after.RowVersion)
	}
	eventsAfterNoOps, err := te.store.GetEvents(ctx, issue.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(eventsAfterNoOps) != len(eventsBeforeNoOps)+2 {
		t.Fatalf("row-idempotent label operations emitted %d new audit events, want 2", len(eventsAfterNoOps)-len(eventsBeforeNoOps))
	}

	target := &types.Issue{ID: "revision-noop-target", Title: "target", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}
	if err := te.store.CreateIssue(ctx, target, "writer"); err != nil {
		t.Fatal(err)
	}
	dep := &types.Dependency{IssueID: issue.ID, DependsOnID: target.ID, Type: types.DepRelated, Metadata: `{"reason":"same"}`}
	if err := te.store.AddDependency(ctx, dep, "writer"); err != nil {
		t.Fatal(err)
	}
	stored, err = te.store.GetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := te.store.AddDependency(ctx, dep, "writer"); err != nil {
		t.Fatal(err)
	}
	after, err = te.store.GetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.RowVersion != stored.RowVersion {
		t.Fatalf("idempotent dependency assertion changed RowVersion %d -> %d", stored.RowVersion, after.RowVersion)
	}

	emptySource := &types.Issue{ID: "revision-empty-metadata-source", Title: "source", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}
	if err := te.store.CreateIssue(ctx, emptySource, "writer"); err != nil {
		t.Fatal(err)
	}
	emptyMetadataDep := &types.Dependency{IssueID: emptySource.ID, DependsOnID: target.ID, Type: types.DepRelated}
	if err := te.store.AddDependency(ctx, emptyMetadataDep, "writer"); err != nil {
		t.Fatal(err)
	}
	stored, err = te.store.GetIssue(ctx, emptySource.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := te.store.AddDependency(ctx, emptyMetadataDep, "writer"); err != nil {
		t.Fatal(err)
	}
	after, err = te.store.GetIssue(ctx, emptySource.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.RowVersion != stored.RowVersion {
		t.Fatalf("empty dependency metadata re-assertion changed RowVersion %d -> %d", stored.RowVersion, after.RowVersion)
	}
}

func TestAggregateRevisionTransactionLabelEventsDoNotRemintNoOps(t *testing.T) {
	skipUnlessEmbeddedDolt(t)
	te := newTestEnv(t, "aggregate_revision_tx_labels")
	ctx := t.Context()
	issue := &types.Issue{ID: "revision-tx-labels", Title: "transaction labels", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}
	if err := te.store.CreateIssue(ctx, issue, "writer"); err != nil {
		t.Fatal(err)
	}

	if err := te.store.RunInTransaction(ctx, "test: add label", func(tx storage.Transaction) error {
		return tx.AddLabel(ctx, issue.ID, "same", "writer")
	}); err != nil {
		t.Fatal(err)
	}
	withLabel, err := te.store.GetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatal(err)
	}
	var committedLabelCount int
	te.queryScalar(t, ctx, "SELECT COUNT(*) FROM labels AS OF 'HEAD' WHERE issue_id = ? AND label = ?", []any{issue.ID, "same"}, &committedLabelCount)
	if committedLabelCount != 1 {
		t.Fatalf("committed label count = %d, want 1", committedLabelCount)
	}
	var committedRevision int64
	te.queryScalar(t, ctx, "SELECT row_lock FROM issues AS OF 'HEAD' WHERE id = ?", []any{issue.ID}, &committedRevision)
	if committedRevision != withLabel.RowVersion {
		t.Fatalf("committed revision = %d, working revision = %d", committedRevision, withLabel.RowVersion)
	}

	if err := te.store.RunInTransaction(ctx, "test: audit row-level label no-ops", func(tx storage.Transaction) error {
		if err := tx.AddLabel(ctx, issue.ID, "same", "writer"); err != nil {
			return err
		}
		return tx.RemoveLabel(ctx, issue.ID, "missing", "writer")
	}); err != nil {
		t.Fatal(err)
	}
	afterNoOps, err := te.store.GetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterNoOps.RowVersion != withLabel.RowVersion {
		t.Fatalf("transactional label no-ops changed RowVersion %d -> %d", withLabel.RowVersion, afterNoOps.RowVersion)
	}
	events, err := te.store.GetEvents(ctx, issue.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var added, removed int
	for _, event := range events {
		switch event.EventType {
		case types.EventLabelAdded:
			added++
		case types.EventLabelRemoved:
			removed++
		}
	}
	if added != 2 || removed != 1 {
		t.Fatalf("transactional label audit events: added=%d removed=%d, want added=2 removed=1", added, removed)
	}

	if err := te.store.RunInTransaction(ctx, "test: remove label", func(tx storage.Transaction) error {
		return tx.RemoveLabel(ctx, issue.ID, "same", "writer")
	}); err != nil {
		t.Fatal(err)
	}
	afterRemove, err := te.store.GetIssue(ctx, issue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterRemove.RowVersion == afterNoOps.RowVersion {
		t.Fatalf("genuine transactional label removal preserved RowVersion %d", afterRemove.RowVersion)
	}
	te.queryScalar(t, ctx, "SELECT COUNT(*) FROM labels AS OF 'HEAD' WHERE issue_id = ? AND label = ?", []any{issue.ID, "same"}, &committedLabelCount)
	if committedLabelCount != 0 {
		t.Fatalf("committed label count after removal = %d, want 0", committedLabelCount)
	}
	te.queryScalar(t, ctx, "SELECT row_lock FROM issues AS OF 'HEAD' WHERE id = ?", []any{issue.ID}, &committedRevision)
	if committedRevision != afterRemove.RowVersion {
		t.Fatalf("committed revision after removal = %d, working revision = %d", committedRevision, afterRemove.RowVersion)
	}
}

func TestAggregateRevisionAuxiliaryUpdatesCommitTheRemintedToken(t *testing.T) {
	skipUnlessEmbeddedDolt(t)
	te := newTestEnv(t, "aggregate_revision_commit")
	ctx := t.Context()
	ops, err := embeddeddolt.NewIssueOperations(te.store)
	if err != nil {
		t.Fatal(err)
	}
	create := func(id string) {
		t.Helper()
		if err := te.store.CreateIssue(ctx, &types.Issue{
			ID: id, Title: id, Status: types.StatusOpen,
			Priority: 2, IssueType: types.TypeTask,
		}, "seed"); err != nil {
			t.Fatal(err)
		}
	}
	readVersions := func(id string) (working, head int64) {
		t.Helper()
		te.queryScalar(t, ctx, "SELECT row_lock FROM issues WHERE id = ?", []any{id}, &working)
		te.queryScalar(t, ctx, "SELECT row_lock FROM issues AS OF 'HEAD' WHERE id = ?", []any{id}, &head)
		return working, head
	}

	create("revision-commit-label")
	create("revision-commit-child")
	create("revision-commit-parent")
	if err := te.store.CloseIssue(ctx, "revision-commit-parent", "done", "seed", ""); err != nil {
		t.Fatal(err)
	}
	if err := te.store.Commit(ctx, "seed"); err != nil {
		t.Fatal(err)
	}

	result, err := ops.Update(ctx, publicops.UpdateRequest{
		Actor: "writer", IssueID: "revision-commit-label",
		Patch: publicops.IssuePatch{Labels: publicops.LabelPatch{Add: []string{"fresh"}}},
	})
	if err != nil || !result.Changed {
		t.Fatalf("label-only update = %#v, %v", result, err)
	}
	working, head := readVersions("revision-commit-label")
	if working != head {
		t.Fatalf("label-only remint was not committed: working=%d HEAD=%d", working, head)
	}

	result, err = ops.Update(ctx, publicops.UpdateRequest{
		Actor: "writer", IssueID: "revision-commit-child",
		Patch: publicops.IssuePatch{ParentID: publicops.Field[string]{Set: true, Value: "revision-commit-parent"}},
	})
	if err != nil || !result.Changed {
		t.Fatalf("parent-only update = %#v, %v", result, err)
	}
	working, head = readVersions("revision-commit-child")
	if working != head {
		t.Fatalf("parent-only remint was not committed: working=%d HEAD=%d", working, head)
	}
}

func mustNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
