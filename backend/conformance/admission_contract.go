package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
	publicops "github.com/steveyegge/beads/issueops"
)

// AdmissionFixture joins the optional durable source and transition roles.
// The raw query hook observes persisted state independently of role answers.
// Authorize supplies a real signed permit and its trusted verification context;
// it does not replace the backend's authorization or transaction machinery.
type AdmissionFixture struct {
	IssuePrefix   string
	Source        publicops.SourceSnapshotReader
	Transition    publicops.RevisionTransition
	Patcher       publicops.RevisionTransitionPatcher
	Verifier      publicops.ProtectedMutationVerifier
	Authorize     func(context.Context, publicops.RevisionTransitionPatchRequest) (context.Context, publicops.ProtectedMutationPermit, publicops.ProtectedMutationScope)
	CreateIssue   func(context.Context, *types.Issue, string) error
	AddDependency func(context.Context, *types.Dependency, string) error
	QueryScalar   func(context.Context, string, []any, ...any) error
}

// RunAdmissionPersistsSignedPatchAndImmutableReceipts pins source completeness,
// exact revision refusal, signed scope, atomic patch persistence and immutable
// retry receipts. Store and unit-of-work legs share SQL bodies: their runs
// qualify adapter wiring, not independent storage implementations.
func RunAdmissionPersistsSignedPatchAndImmutableReceipts(t *testing.T, ctx context.Context, fixture AdmissionFixture) {
	t.Helper()
	id := fixture.IssuePrefix + "-source"
	target := fixture.IssuePrefix + "-target"
	for _, issueID := range []string{id, target} {
		issue := &types.Issue{ID: issueID, Title: "admission contract", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask, Metadata: json.RawMessage(`{"retained":"yes"}`)}
		if err := fixture.CreateIssue(ctx, issue, "contract-writer"); err != nil {
			t.Fatal(err)
		}
	}
	if err := fixture.AddDependency(ctx, &types.Dependency{IssueID: id, DependsOnID: target, Type: types.DepBlocks}, "contract-writer"); err != nil {
		t.Fatal(err)
	}
	before, err := fixture.Source.ReadSourceSnapshot(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if before == nil || before.Issue == nil || before.Issue.ID != id || before.Issue.RowVersion == 0 || len(before.OutgoingDependencies) != 1 || before.OutgoingDependencies[0].DependsOnID != target {
		t.Fatalf("source snapshot omits row, revision or outgoing edge: %+v", before)
	}
	empty, err := fixture.Source.ReadSourceSnapshot(ctx, target)
	if err != nil || empty == nil || empty.OutgoingDependencies == nil || len(empty.OutgoingDependencies) != 0 {
		t.Fatalf("empty edge set = %+v, %v", empty, err)
	}
	ready := json.RawMessage(`"ready"`)
	req := publicops.RevisionTransitionRequest{ReceiptID: fixture.IssuePrefix + "-parent", IssueID: id, Scope: "contract", Kind: "ensure", Actor: "contract-writer", ExpectedVersion: before.Issue.RowVersion, Key: "gc_contract", Value: &ready, Payload: json.RawMessage(`{"contract":true}`)}
	parent, err := fixture.Transition.TransitionMetadata(ctx, req)
	if err != nil || !parent.Applied || parent.Replayed || parent.Receipt == nil {
		t.Fatalf("parent transition = %+v, %v", parent, err)
	}
	if parent.Receipt.ToVersion == 0 || parent.Receipt.ToVersion == req.ExpectedVersion {
		t.Fatal("parent did not mint a new revision")
	}
	stale := req
	stale.ReceiptID += "-stale"
	if _, err := fixture.Transition.TransitionMetadata(ctx, stale); !errors.Is(err, storage.ErrVersionMismatch) {
		t.Fatalf("stale transition = %v", err)
	}
	storedParent, err := fixture.Transition.GetReceipt(ctx, req.ReceiptID)
	if err != nil || !reflect.DeepEqual(storedParent, parent.Receipt) {
		t.Fatalf("stored parent differs: %+v, %v", storedParent, err)
	}
	digest, err := publicops.RevisionTransitionReceiptDigest(*parent.Receipt)
	if err != nil {
		t.Fatal(err)
	}
	assigned := json.RawMessage(`"assigned"`)
	patch := publicops.RevisionTransitionPatchRequest{ReceiptID: fixture.IssuePrefix + "-patch", IssueID: id, Scope: req.Scope, Kind: "route", Actor: req.Actor, ExpectedVersion: parent.Receipt.ToVersion, PriorReceiptID: req.ReceiptID, PriorReceiptDigest: digest, Patch: publicops.RevisionTransitionIssuePatch{
		Metadata: []publicops.RevisionTransitionMetadataPatch{{Key: req.Key, Expected: &ready, Value: &assigned}},
		Status:   &publicops.RevisionTransitionStringPatch{Expected: "open", Value: "in_progress"},
		Assignee: &publicops.RevisionTransitionStringPatch{Expected: "", Value: "worker"},
		Labels:   &publicops.RevisionTransitionLabelsPatch{Expected: []string{}, Value: []string{"contract"}},
	}}
	signedCtx, permit, scope := fixture.Authorize(ctx, patch)
	verification, err := fixture.Verifier.Verify(signedCtx, permit, scope)
	if err != nil || verification.ReplayID == "" {
		t.Fatalf("valid signed permit = %+v, %v", verification, err)
	}
	wrong := scope
	wrong.ResourceIDs = []string{target}
	if _, err := fixture.Verifier.Verify(signedCtx, permit, wrong); err == nil {
		t.Fatal("permit authorized a different resource")
	}
	if _, err := fixture.Verifier.Verify(signedCtx, "invalid", scope); err == nil {
		t.Fatal("verifier accepted an invalid signature")
	}
	patch.ProtectedPermit = "invalid"
	if _, err := fixture.Patcher.TransitionPatch(signedCtx, patch); err == nil {
		t.Fatal("patch accepted an invalid permit")
	}
	var revision int64
	if err := fixture.QueryScalar(ctx, "SELECT row_lock FROM issues WHERE id = ?", []any{id}, &revision); err != nil {
		t.Fatal(err)
	}
	if revision != parent.Receipt.ToVersion {
		t.Fatal("refused patch changed the source revision")
	}
	patch.ProtectedPermit = permit
	applied, err := fixture.Patcher.TransitionPatch(signedCtx, patch)
	if err != nil || !applied.Applied || applied.Replayed || applied.Receipt == nil {
		t.Fatalf("signed patch = %+v, %v", applied, err)
	}
	var status, assignee, marker, retained string
	if err := fixture.QueryScalar(ctx, "SELECT row_lock, status, assignee, JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.gc_contract')), JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.retained')) FROM issues WHERE id = ?", []any{id}, &revision, &status, &assignee, &marker, &retained); err != nil {
		t.Fatal(err)
	}
	if revision != applied.Receipt.ToVersion || revision == parent.Receipt.ToVersion || status != "in_progress" || assignee != "worker" || marker != "assigned" || retained != "yes" {
		t.Fatalf("persisted patch = %d %s %s %s %s", revision, status, assignee, marker, retained)
	}
	var labels int
	if err := fixture.QueryScalar(ctx, "SELECT COUNT(*) FROM labels WHERE issue_id = ? AND label = ?", []any{id, "contract"}, &labels); err != nil {
		t.Fatal(err)
	}
	if labels != 1 {
		t.Fatalf("persisted labels = %d", labels)
	}
	stored, err := fixture.Patcher.GetPatchReceipt(ctx, patch.ReceiptID)
	if err != nil || !reflect.DeepEqual(stored, applied.Receipt) {
		t.Fatalf("stored patch differs: %+v, %v", stored, err)
	}
	patch.ProtectedPermit = "invalid-on-retry"
	replay, err := fixture.Patcher.TransitionPatch(ctx, patch)
	if err != nil || !replay.Replayed || !reflect.DeepEqual(replay.Receipt, applied.Receipt) {
		t.Fatalf("patch replay = %+v, %v", replay, err)
	}
	parentReplay, err := fixture.Transition.TransitionMetadata(ctx, req)
	if err != nil || !parentReplay.Replayed || !reflect.DeepEqual(parentReplay.Receipt, parent.Receipt) {
		t.Fatalf("parent replay = %+v, %v", parentReplay, err)
	}
	var receipts, uses int
	if err := fixture.QueryScalar(ctx, "SELECT COUNT(*) FROM issue_transition_receipts WHERE issue_id = ?", []any{id}, &receipts); err != nil {
		t.Fatal(err)
	}
	if err := fixture.QueryScalar(ctx, "SELECT COUNT(*) FROM protected_mutation_permit_uses WHERE replay_id = ?", []any{verification.ReplayID}, &uses); err != nil {
		t.Fatal(err)
	}
	if receipts != 2 || uses != 1 {
		t.Fatalf("receipt/use counts = %d/%d, want 2/1", receipts, uses)
	}
	if err := fixture.QueryScalar(ctx, "SELECT row_lock FROM issues WHERE id = ?", []any{id}, &revision); err != nil {
		t.Fatal(err)
	}
	if revision != applied.Receipt.ToVersion {
		t.Fatal("replay mutated the source revision")
	}
}
