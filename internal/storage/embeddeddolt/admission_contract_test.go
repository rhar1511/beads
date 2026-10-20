//go:build cgo

package embeddeddolt_test

import (
	"testing"

	"github.com/rhar1511/beads/backend/conformance"
	"github.com/rhar1511/beads/internal/testutil/admissionauthority"
	publicops "github.com/rhar1511/beads/issueops"
)

func TestAdmissionContract(t *testing.T) {
	skipUnlessEmbeddedDolt(t)
	te := newTestEnv(t, "adm")
	ctx := t.Context()
	transition, err := te.store.RevisionTransition()
	if err != nil {
		t.Fatal(err)
	}
	reader, err := te.store.SourceSnapshotReader()
	if err != nil {
		t.Fatal(err)
	}
	kit := newEmbeddedRoleFixtureKit(te, "adm")
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
