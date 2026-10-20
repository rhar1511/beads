package dolt

import (
	"context"
	"testing"

	"github.com/rhar1511/beads/backend/conformance"
	"github.com/rhar1511/beads/internal/testutil/admissionauthority"
	publicops "github.com/rhar1511/beads/issueops"
)

func TestAdmissionContract(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()
	runDoltAdmissionContract(t, ctx, store)
}

func runDoltAdmissionContract(t *testing.T, ctx context.Context, store *DoltStore) {
	t.Helper()
	transition, err := store.RevisionTransition()
	if err != nil {
		t.Fatal(err)
	}
	reader, err := store.SourceSnapshotReader()
	if err != nil {
		t.Fatal(err)
	}
	kit := newDoltRoleFixtureKit(store, "adm")
	patcher, ok := transition.(publicops.RevisionTransitionPatcher)
	if !ok {
		t.Fatalf("transition %T lacks patch capability", transition)
	}
	authority := admissionauthority.New(t)
	conformance.RunAdmissionPersistsSignedPatchAndImmutableReceipts(t, ctx, conformance.AdmissionFixture{
		IssuePrefix: kit.IssuePrefix, Source: reader, Transition: transition, Patcher: patcher,
		Verifier: authority.Verifier, Authorize: authority.Authorizer(t),
		CreateIssue: kit.CreateIssue, AddDependency: kit.AddDependency, QueryScalar: kit.QueryScalar,
	})
}
