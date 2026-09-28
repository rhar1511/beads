package uow

import (
	"context"
	"errors"
	"fmt"

	"github.com/steveyegge/beads/internal/storage"
	storeops "github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/storage/schema"
	publicops "github.com/steveyegge/beads/issueops"
)

// RevisionTransitionSource is the capability accessor a unit-of-work provider
// offers for issue revision-fenced metadata transitions.
type RevisionTransitionSource interface {
	RevisionTransition() (publicops.RevisionTransition, error)
}

// RevisionTransitionReadinessSource reports whether this provider's selected
// database has every migration and backfill required by the transition role.
type RevisionTransitionReadinessSource interface {
	RevisionTransitionReadiness(context.Context) (schema.RevisionTransitionReadiness, error)
}

type revisionTransition struct {
	provider UnitOfWorkProvider
}

func (p *doltSQLProvider) RevisionTransition() (publicops.RevisionTransition, error) {
	return NewRevisionTransition(p)
}

// RevisionTransitionReadiness probes the exact schema and backfill state for
// this provider's selected database. A probe error is not readiness.
func (p *doltSQLProvider) RevisionTransitionReadiness(ctx context.Context) (schema.RevisionTransitionReadiness, error) {
	if p == nil || p.db == nil {
		return schema.RevisionTransitionReadiness{}, &storage.ErrUnsupported{Op: "RevisionTransitionReadiness", Backend: "nil"}
	}
	return schema.ProbeRevisionTransitionReadiness(ctx, p.db)
}

// NewRevisionTransition constructs the transition role backed by provider.
func NewRevisionTransition(provider UnitOfWorkProvider) (publicops.RevisionTransition, error) {
	if isNilUnitOfWorkProvider(provider) {
		return nil, fmt.Errorf("new revision transition: unit-of-work provider must not be nil")
	}
	return &revisionTransition{provider: provider}, nil
}

var _ publicops.RevisionTransition = (*revisionTransition)(nil)

func (r *revisionTransition) TransitionMetadata(ctx context.Context, req publicops.RevisionTransitionRequest) (publicops.RevisionTransitionResult, error) {
	plan, err := storage.PlanRevisionTransition(req)
	if err != nil {
		return publicops.RevisionTransitionResult{}, err
	}
	digest, err := publicops.ProtectedMutationDigest(req)
	if err != nil {
		return publicops.RevisionTransitionResult{}, err
	}
	var preflight publicops.RevisionTransitionResult
	var preflightDone bool
	_, err = RunTxRead(ctx, r.provider, func(ctx context.Context, uw UnitOfWork) (struct{}, error) {
		receipt, err := uw.IssueUseCase().GetRevisionTransitionReceipt(ctx, plan.ReceiptID)
		if err == nil {
			if !storage.SameRevisionTransitionRequest(plan, receipt) {
				return struct{}{}, fmt.Errorf("%w: receipt %s is bound to another request", publicops.ErrTransitionReceiptConflict, plan.ReceiptID)
			}
			preflight = publicops.RevisionTransitionResult{Applied: true, Replayed: true, Receipt: receipt}
			preflightDone = true
			return struct{}{}, nil
		}
		if !errors.Is(err, storage.ErrNotFound) {
			return struct{}{}, err
		}
		if err := validateRevisionTransitionSource(ctx, uw, plan.IssueID); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, nil
	})
	if err != nil {
		return publicops.RevisionTransitionResult{}, err
	}
	if preflightDone {
		return preflight, nil
	}
	for attempt := 0; attempt < 3; attempt++ {
		result, err := RunTxResult(ctx, r.provider, func(ctx context.Context, uw UnitOfWork) (publicops.RevisionTransitionResult, string, error) {
			if err := requireProtectedUOWMutation(ctx, uw, req.ProtectedPermit, "issue.revision_transition", []string{plan.IssueID}, digest, ""); err != nil {
				return publicops.RevisionTransitionResult{}, "", err
			}
			transition, wrote, err := uw.IssueUseCase().TransitionMetadata(ctx, plan)
			if err != nil || !wrote {
				return transition, "", err
			}
			return transition, fmt.Sprintf("bd: revision transition %s", plan.ReceiptID), nil
		})
		if errors.Is(err, storeops.ErrRevisionTransitionReceiptRace) {
			continue
		}
		return result, err
	}
	return publicops.RevisionTransitionResult{}, fmt.Errorf("revision transition receipt %s did not converge after concurrent receipt creation: %w", plan.ReceiptID, publicops.ErrTransitionReceiptConflict)
}

func validateRevisionTransitionSource(ctx context.Context, uw UnitOfWork, issueID string) error {
	deps := uw.DependencyUseCase()
	wispSources, err := deps.WispSourceIDs(ctx, []string{issueID})
	if err != nil {
		return fmt.Errorf("check revision transition source plane for %s: %w", issueID, err)
	}
	if _, ok := wispSources[issueID]; ok {
		return revisionTransitionWispUnsupported("source issue is stored in wisps")
	}
	if _, err := uw.IssueUseCase().GetIssue(ctx, issueID); err != nil {
		return err
	}
	wispDeps, err := deps.GetWispDependencyRecords(ctx, []string{issueID})
	if err != nil {
		return fmt.Errorf("check revision transition wisp dependencies for %s: %w", issueID, err)
	}
	if len(wispDeps[issueID]) != 0 {
		return revisionTransitionWispUnsupported("source has outgoing wisp dependency rows")
	}
	issueDeps, err := deps.GetIssueDependencyRecords(ctx, []string{issueID})
	if err != nil {
		return fmt.Errorf("check revision transition dependencies for %s: %w", issueID, err)
	}
	targets := make([]string, 0, len(issueDeps[issueID]))
	seen := make(map[string]struct{}, len(issueDeps[issueID]))
	for _, dep := range issueDeps[issueID] {
		if dep == nil || dep.DependsOnID == "" {
			continue
		}
		if _, ok := seen[dep.DependsOnID]; ok {
			continue
		}
		seen[dep.DependsOnID] = struct{}{}
		targets = append(targets, dep.DependsOnID)
	}
	if len(targets) == 0 {
		return nil
	}
	wispTargets, err := deps.WispSourceIDs(ctx, targets)
	if err != nil {
		return fmt.Errorf("check revision transition dependency targets for %s: %w", issueID, err)
	}
	if len(wispTargets) != 0 {
		return revisionTransitionWispUnsupported("source has a wisp-backed dependency target")
	}
	return nil
}

func revisionTransitionWispUnsupported(reason string) error {
	return fmt.Errorf("%w: %s", &storage.ErrUnsupported{Op: "RevisionTransition", Backend: "Dolt wisp plane"}, reason)
}

func (r *revisionTransition) GetReceipt(ctx context.Context, receiptID string) (*publicops.RevisionTransitionReceipt, error) {
	if err := storage.ValidateRevisionTransitionReceiptID(receiptID); err != nil {
		return nil, err
	}
	return RunTxRead(ctx, r.provider, func(ctx context.Context, uw UnitOfWork) (*publicops.RevisionTransitionReceipt, error) {
		return uw.IssueUseCase().GetRevisionTransitionReceipt(ctx, receiptID)
	})
}
