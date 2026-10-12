package uow

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"

	"github.com/steveyegge/beads/internal/types"
)

// The real provider test proves that the marker and receipt survive together,
// including when the request is replayed after its original response is lost.
func TestMetadataTransitionCommitsMarkerAndReceiptAtomically(t *testing.T) {
	ctx := context.Background()
	provider := newUOWRoleFixtureProvider(t, ctx, "transition")
	kit := newUOWRoleFixtureKit(provider, "transition")
	id := kit.IssuePrefix + "-source"
	if err := kit.CreateIssue(ctx, &types.Issue{
		ID: id, Title: "transition source", Status: types.StatusOpen,
		Priority: 2, IssueType: types.TypeTask,
	}, "transition-seed"); err != nil {
		t.Fatal(err)
	}
	read := func() *types.Issue {
		t.Helper()
		issue, err := RunTxRead(ctx, provider, func(ctx context.Context, uw UnitOfWork) (*types.Issue, error) {
			return uw.IssueUseCase().GetIssue(ctx, id)
		})
		if err != nil {
			t.Fatal(err)
		}
		return issue
	}
	before := read()
	value := json.RawMessage(`"held"`)
	req := MetadataTransitionRequest{
		ReceiptID: kit.IssuePrefix + "-receipt", IssueID: id,
		Scope: "rig:test", Kind: "hold", Actor: "transition-test",
		ExpectedVersion: strconv.FormatInt(before.RowVersion, 10),
		Key:             "gc.hold", Value: &value, Payload: json.RawMessage(`{"case":"commit"}`),
	}
	first, err := TransitionMetadata(ctx, provider, req)
	if err != nil {
		t.Fatalf("first transition: %v", err)
	}
	if !first.Applied || first.Replayed || first.Receipt == nil || first.Receipt.ToVersion == req.ExpectedVersion {
		t.Fatalf("first result = %+v", first)
	}
	after := read()
	if strconv.FormatInt(after.RowVersion, 10) != first.Receipt.ToVersion {
		t.Fatalf("row revision = %d, receipt to_version = %s", after.RowVersion, first.Receipt.ToVersion)
	}
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(after.Metadata, &metadata); err != nil || string(metadata["gc.hold"]) != `"held"` {
		t.Fatalf("committed marker = %s, error = %v", after.Metadata, err)
	}
	stored, found, err := MetadataTransitionReceiptByID(ctx, provider, req.ReceiptID)
	if err != nil || !found || stored.ToVersion != first.Receipt.ToVersion || stored.IssueID != id {
		t.Fatalf("durable receipt = %+v, found=%t, err=%v", stored, found, err)
	}
	replay, err := TransitionMetadata(ctx, provider, req)
	if err != nil || !replay.Applied || !replay.Replayed || replay.Receipt == nil || replay.Receipt.ToVersion != stored.ToVersion {
		t.Fatalf("exact replay = %+v, err=%v", replay, err)
	}
	if got := read().RowVersion; got != after.RowVersion {
		t.Fatalf("replay changed row revision: before=%d after=%d", after.RowVersion, got)
	}
	wrong := req
	wrong.Kind = "other"
	if _, err := TransitionMetadata(ctx, provider, wrong); !errors.Is(err, ErrTransitionReceiptConflict) {
		t.Fatalf("receipt ID reuse error = %v, want ErrTransitionReceiptConflict", err)
	}
	stale := req
	stale.ReceiptID += "-stale"
	if _, err := TransitionMetadata(ctx, provider, stale); !errors.Is(err, ErrTransitionPrecondition) {
		t.Fatalf("stale revision error = %v, want ErrTransitionPrecondition", err)
	}
}
