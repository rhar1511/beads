package uow

import (
	"testing"

	"github.com/rhar1511/beads/backend/conformance"
	"github.com/rhar1511/beads/internal/testutil/admissionauthority"
	publicops "github.com/rhar1511/beads/issueops"
)

func TestAdmissionContract(t *testing.T) {
	ctx := t.Context()
	provider := newUOWRoleFixtureProvider(t, ctx, "adm")
	transitions, ok := provider.(RevisionTransitionSource)
	if !ok {
		t.Fatalf("provider %T lacks RevisionTransition", provider)
	}
	transition, err := transitions.RevisionTransition()
	if err != nil {
		t.Fatal(err)
	}
	snapshots, ok := provider.(SourceSnapshotReaderSource)
	if !ok {
		t.Fatalf("provider %T lacks SourceSnapshotReader", provider)
	}
	reader, err := snapshots.SourceSnapshotReader()
	if err != nil {
		t.Fatal(err)
	}
	kit := newUOWRoleFixtureKit(provider, "adm")
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
