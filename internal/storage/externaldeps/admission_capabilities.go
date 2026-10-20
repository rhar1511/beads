package externaldeps

import (
	"context"

	"github.com/rhar1511/beads/internal/storage"
	"github.com/rhar1511/beads/internal/storage/domain"
	"github.com/rhar1511/beads/internal/storage/schema"
	"github.com/rhar1511/beads/internal/storage/uow"
	publicops "github.com/rhar1511/beads/issueops"
)

// Admission capabilities are additive to the ordinary storage interfaces.
// Preserve their exact proofs and errors through the external-dependency
// policy; the backend continues to own permit enforcement and atomic writes.
func (s *Store) RevisionTransitionReadiness(ctx context.Context) (schema.RevisionTransitionReadiness, error) {
	source, ok := s.inner.(uow.RevisionTransitionReadinessSource)
	if !ok {
		return schema.RevisionTransitionReadiness{}, &storage.ErrUnsupported{Op: "RevisionTransitionReadiness", Backend: "external dependency wrapper"}
	}
	return source.RevisionTransitionReadiness(ctx)
}

func (s *Store) BatchApplyReceiptReadiness(ctx context.Context) (schema.BatchApplyReceiptReadiness, error) {
	source, ok := s.inner.(uow.BatchApplyReceiptReadinessSource)
	if !ok {
		return schema.BatchApplyReceiptReadiness{}, &storage.ErrUnsupported{Op: "BatchApplyReceiptReadiness", Backend: "external dependency wrapper"}
	}
	return source.BatchApplyReceiptReadiness(ctx)
}

func (s *Store) ProtectedMutationReadiness(ctx context.Context) (schema.ProtectedMutationReadiness, error) {
	source, ok := s.inner.(uow.ProtectedMutationReadinessSource)
	if !ok {
		return schema.ProtectedMutationReadiness{}, &storage.ErrUnsupported{Op: "ProtectedMutationReadiness", Backend: "external dependency wrapper"}
	}
	return source.ProtectedMutationReadiness(ctx)
}

func (p *uowProvider) RevisionTransitionReadiness(ctx context.Context) (schema.RevisionTransitionReadiness, error) {
	source, ok := p.UnitOfWorkProvider.(uow.RevisionTransitionReadinessSource)
	if !ok {
		return schema.RevisionTransitionReadiness{}, &storage.ErrUnsupported{Op: "RevisionTransitionReadiness", Backend: "external dependency wrapper"}
	}
	return source.RevisionTransitionReadiness(ctx)
}

func (p *uowProvider) BatchApplyReceiptReadiness(ctx context.Context) (schema.BatchApplyReceiptReadiness, error) {
	source, ok := p.UnitOfWorkProvider.(uow.BatchApplyReceiptReadinessSource)
	if !ok {
		return schema.BatchApplyReceiptReadiness{}, &storage.ErrUnsupported{Op: "BatchApplyReceiptReadiness", Backend: "external dependency wrapper"}
	}
	return source.BatchApplyReceiptReadiness(ctx)
}

func (p *uowProvider) ProtectedMutationReadiness(ctx context.Context) (schema.ProtectedMutationReadiness, error) {
	source, ok := p.UnitOfWorkProvider.(uow.ProtectedMutationReadinessSource)
	if !ok {
		return schema.ProtectedMutationReadiness{}, &storage.ErrUnsupported{Op: "ProtectedMutationReadiness", Backend: "external dependency wrapper"}
	}
	return source.ProtectedMutationReadiness(ctx)
}

func (s *Store) RevisionTransition() (publicops.RevisionTransition, error) {
	source, ok := s.inner.(uow.RevisionTransitionSource)
	if !ok {
		return nil, &storage.ErrUnsupported{Op: "RevisionTransition", Backend: "external dependency wrapper"}
	}
	return source.RevisionTransition()
}

func (p *uowProvider) RevisionTransition() (publicops.RevisionTransition, error) {
	return uow.NewRevisionTransition(p)
}

func (s *Store) SourceSnapshotReader() (publicops.SourceSnapshotReader, error) {
	source, ok := s.inner.(uow.SourceSnapshotReaderSource)
	if !ok {
		return nil, &storage.ErrUnsupported{Op: "SourceSnapshotReader", Backend: "external dependency wrapper"}
	}
	return source.SourceSnapshotReader()
}

func (p *uowProvider) SourceSnapshotReader() (publicops.SourceSnapshotReader, error) {
	return uow.NewSourceSnapshotReader(p)
}

func (u *issueUseCase) TransitionPatch(ctx context.Context, plan storage.RevisionTransitionPatchPlan) (publicops.RevisionTransitionPatchResult, bool, error) {
	patcher, ok := u.IssueUseCase.(domain.RevisionTransitionPatchUseCase)
	if !ok {
		return publicops.RevisionTransitionPatchResult{}, false, &storage.ErrUnsupported{Op: "RevisionTransitionPatch", Backend: "external dependency issue use case"}
	}
	return patcher.TransitionPatch(ctx, plan)
}

func (u *issueUseCase) GetRevisionTransitionPatchReceipt(ctx context.Context, receiptID string) (*publicops.RevisionTransitionPatchReceipt, error) {
	patcher, ok := u.IssueUseCase.(domain.RevisionTransitionPatchUseCase)
	if !ok {
		return nil, &storage.ErrUnsupported{Op: "RevisionTransitionPatchReceipt", Backend: "external dependency issue use case"}
	}
	return patcher.GetRevisionTransitionPatchReceipt(ctx, receiptID)
}
