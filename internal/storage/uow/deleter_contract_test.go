package uow

import (
	"context"
	"errors"
	"testing"

	"github.com/rhar1511/beads/backend/conformance"
	publicops "github.com/rhar1511/beads/issueops"
)

// TestDeleterContract runs the Deleter contract against the unit-of-work
// provider — the one implementation that does not run
// internal/storage/issueops.DeleteInTx. It is the SECOND of two votes, not the
// third: the two store backends share the other body.
//
// It is also the backend this role changes most: the proxied route hardcoded
// cascade at both of its call sites and REFUSED `--cascade` as an unsupported
// flag, so there was no way to delete an issue on a team server without taking
// its dependents. The guard and the orphan mode below are new behaviour here.
//
// One provider for the whole suite (each newUOWRoleFixtureProvider boots a real
// Dolt sql-server) and NO t.Parallel: this backend has no per-test
// copy-on-write branch, so dolt_log and issues are database-global.
func TestDeleterContract(t *testing.T) {
	ctx := context.Background()
	fixture := newUOWDeleterFixture(t, ctx, "del")

	t.Run("RefusesAMalformedRequest", func(t *testing.T) {
		conformance.RunDeleterRefusesAMalformedRequest(t, ctx, fixture)
	})
	t.Run("RefusesAnAbsentID", func(t *testing.T) {
		conformance.RunDeleterRefusesAnAbsentID(t, ctx, fixture)
	})
	t.Run("RefusesDependentsOutsideTheRequest", func(t *testing.T) {
		conformance.RunDeleterRefusesDependentsOutsideTheRequest(t, ctx, fixture)
	})
	t.Run("ProtectsActiveReferences", func(t *testing.T) {
		conformance.RunDeleterProtectsActiveReferences(t, ctx, fixture)
	})
	t.Run("ProtectsOnlySurvivingOwners", func(t *testing.T) {
		conformance.RunDeleterProtectsOnlySurvivingOwners(t, ctx, fixture)
	})
	t.Run("FencesOpenTargetWispOwner", func(t *testing.T) {
		conformance.RunDeleterFencesOpenTargetWispOwner(t, ctx, fixture)
	})
	t.Run("ForceOrphansDependents", func(t *testing.T) {
		conformance.RunDeleterForceOrphansDependents(t, ctx, fixture)
	})
	t.Run("CascadeDeletesTheClosure", func(t *testing.T) {
		conformance.RunDeleterCascadeDeletesTheClosure(t, ctx, fixture)
	})
	t.Run("ErasesAcrossBothPlanes", func(t *testing.T) {
		conformance.RunDeleterErasesAcrossBothPlanes(t, ctx, fixture)
	})
	t.Run("CascadeFromAWispRootDeletesTheClosure", func(t *testing.T) {
		conformance.RunDeleterCascadeFromAWispRootDeletesTheClosure(t, ctx, fixture)
	})
	t.Run("GuardsAWispNamedWithADurableDependent", func(t *testing.T) {
		conformance.RunDeleterGuardsAWispNamedWithADurableDependent(t, ctx, fixture)
	})
	t.Run("GuardsADurableNamedWithAWispDependent", func(t *testing.T) {
		conformance.RunDeleterGuardsADurableNamedWithAWispDependent(t, ctx, fixture)
	})
	t.Run("CountsCrossPlaneEdgesItRemoves", func(t *testing.T) {
		conformance.RunDeleterCountsCrossPlaneEdgesItRemoves(t, ctx, fixture)
	})
	t.Run("NeverCallsALiveRowDeleted", func(t *testing.T) {
		conformance.RunDeleterNeverCallsALiveRowDeleted(t, ctx, fixture)
	})
	t.Run("CollapsesDuplicateIDs", func(t *testing.T) {
		conformance.RunDeleterCollapsesDuplicateIDs(t, ctx, fixture)
	})
	t.Run("RewritesReferencesInNeighbors", func(t *testing.T) {
		conformance.RunDeleterRewritesReferencesInNeighbors(t, ctx, fixture)
	})
	t.Run("DryRunChangesNothing", func(t *testing.T) {
		conformance.RunDeleterDryRunChangesNothing(t, ctx, fixture)
	})
	t.Run("RecordsExactlyOneHistoryEntry", func(t *testing.T) {
		conformance.RunDeleterRecordsExactlyOneHistoryEntry(t, ctx, fixture)
	})
	t.Run("DoesNotMutateTheCallerRequest", func(t *testing.T) {
		conformance.RunDeleterDoesNotMutateTheCallerRequest(t, ctx, fixture)
	})
	t.Run("SettlesTheSurvivorsOfADeletedBlocker", func(t *testing.T) {
		conformance.RunDeleterSettlesTheSurvivorsOfADeletedBlocker(t, ctx, fixture)
	})
	t.Run("SettlesTheSurvivorsOfADeletedWispBlocker", func(t *testing.T) {
		conformance.RunDeleterSettlesTheSurvivorsOfADeletedWispBlocker(t, ctx, fixture)
	})
	t.Run("SettlesTheChildrenOfADeletedParent", func(t *testing.T) {
		conformance.RunDeleterSettlesTheChildrenOfADeletedParent(t, ctx, fixture)
	})
	t.Run("DeletesOnAMatchingExpectedVersion", func(t *testing.T) {
		conformance.RunDeleterDeletesOnAMatchingExpectedVersion(t, ctx, fixture)
	})
	t.Run("RefusesAStaleExpectedVersion", func(t *testing.T) {
		conformance.RunDeleterRefusesAStaleExpectedVersion(t, ctx, fixture)
	})
	t.Run("VersionOutranksForceAndCascade", func(t *testing.T) {
		conformance.RunDeleterVersionOutranksForceAndCascade(t, ctx, fixture)
	})
	t.Run("RefusesAnExpectedVersionAcrossSeveralIDs", func(t *testing.T) {
		conformance.RunDeleterRefusesAnExpectedVersionAcrossSeveralIDs(t, ctx, fixture)
	})
}

func newUOWDeleterFixture(t *testing.T, ctx context.Context, prefix string) conformance.DeleterFixture {
	t.Helper()
	provider := newUOWRoleFixtureProvider(t, ctx, prefix)
	// Through the capability accessor, not NewDeleter: a provider that stopped
	// offering the role is the regression a constructor call would hide.
	source, ok := provider.(DeleterSource)
	if !ok {
		t.Fatalf("provider %T does not offer the Deleter accessor", provider)
	}
	deleter, err := source.Deleter()
	if err != nil {
		t.Fatalf("Deleter(): %v", err)
	}
	kit := newUOWRoleFixtureKit(provider, prefix)
	return conformance.DeleterFixture{
		IssuePrefix:   kit.IssuePrefix,
		Deleter:       deleter,
		CreateIssue:   kit.CreateIssue,
		CreateWisp:    kit.CreateWisp,
		AddDependency: kit.AddDependency,
		QueryScalar:   kit.QueryScalar,
		CountHistory:  kit.CountHistory,
		CommitPending: uowCommitPending(provider),
	}
}

// uowCommitPending settles the working set into the version history. Every seed
// on this backend is its own unit of work and versions itself on the way in, so
// an empty unit of work here has nothing left to publish — doltServerTx.Commit
// demotes a commit with no pending changes to a plain SQL COMMIT. The history
// case still asks for it, because settling is a stated precondition rather than
// a side effect the case is entitled to assume.
func uowCommitPending(provider UnitOfWorkProvider) func(context.Context) error {
	return func(ctx context.Context) error {
		return RunTx(ctx, provider, func(context.Context, UnitOfWork) (string, error) {
			return "conformance: settle seeds before a history delta", nil
		})
	}
}

func TestDeleterActiveReferenceCheckRequiresTransactionalRunner(t *testing.T) {
	uw := &mockUnitOfWork{}
	deleter, err := NewDeleter(&mockUnitOfWorkProvider{uows: []*mockUnitOfWork{uw}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = deleter.Delete(context.Background(), publicops.DeleteRequest{
		IDs: []string{"target"}, Force: true, ProtectActiveReferences: true,
	})
	var unsupported *publicops.ErrUnsupported
	if !errors.As(err, &unsupported) {
		t.Fatalf("missing transactional capability must refuse: %v", err)
	}
	if uw.commitCount != 0 || !uw.closed {
		t.Fatalf("unsupported request committed or leaked unit of work: %+v", uw)
	}
}
