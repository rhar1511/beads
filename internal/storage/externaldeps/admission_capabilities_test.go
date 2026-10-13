package externaldeps

import (
	"context"
	"errors"
	"testing"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/domain"
	"github.com/steveyegge/beads/internal/storage/schema"
	"github.com/steveyegge/beads/internal/storage/uow"
	publicops "github.com/steveyegge/beads/issueops"
)

type admissionProbes struct {
	err   error
	calls int
}

func (p *admissionProbes) RevisionTransitionReadiness(context.Context) (schema.RevisionTransitionReadiness, error) {
	p.calls++
	return schema.RevisionTransitionReadiness{}, p.err
}
func (p *admissionProbes) BatchApplyReceiptReadiness(context.Context) (schema.BatchApplyReceiptReadiness, error) {
	p.calls++
	return schema.BatchApplyReceiptReadiness{}, p.err
}
func (p *admissionProbes) ProtectedMutationReadiness(context.Context) (schema.ProtectedMutationReadiness, error) {
	p.calls++
	return schema.ProtectedMutationReadiness{}, p.err
}
func (p *admissionProbes) RevisionTransition() (publicops.RevisionTransition, error) {
	return nil, p.err
}
func (p *admissionProbes) SourceSnapshotReader() (publicops.SourceSnapshotReader, error) {
	return nil, p.err
}

type admissionStore struct {
	storage.DoltStorage
	*admissionProbes
}
type admissionProvider struct {
	uow.UnitOfWorkProvider
	*admissionProbes
}

// A new policy wrapper must preserve schema proofs and their errors. Losing
// these optional interfaces silently suppresses HTTP transition capabilities.
func TestAdmissionReadinessSurvivesPolicyWrapper(t *testing.T) {
	for _, kind := range []string{"store", "provider"} {
		t.Run(kind, func(t *testing.T) {
			want := errors.New("schema probe failed")
			probes := &admissionProbes{err: want}
			var wrapped any
			if kind == "store" {
				wrapped = New(&admissionStore{admissionProbes: probes}, nil, nil)
			} else {
				wrapped = WrapUOWProvider(&admissionProvider{admissionProbes: probes}, nil, nil)
			}
			revision, ok := wrapped.(uow.RevisionTransitionReadinessSource)
			if !ok {
				t.Fatal("policy wrapper hides revision readiness")
			}
			got, err := revision.RevisionTransitionReadiness(context.Background())
			if got.Ready() || !errors.Is(err, want) {
				t.Fatalf("revision probe: %+v %v", got, err)
			}
			batch, ok := wrapped.(uow.BatchApplyReceiptReadinessSource)
			if !ok {
				t.Fatal("policy wrapper hides batch receipt readiness")
			}
			b, err := batch.BatchApplyReceiptReadiness(context.Background())
			if b.Ready() || !errors.Is(err, want) {
				t.Fatalf("batch probe: %+v %v", b, err)
			}
			protected, ok := wrapped.(uow.ProtectedMutationReadinessSource)
			if !ok {
				t.Fatal("policy wrapper hides protected mutation readiness")
			}
			p, err := protected.ProtectedMutationReadiness(context.Background())
			if p.Ready() || !errors.Is(err, want) {
				t.Fatalf("protected probe: %+v %v", p, err)
			}
			if probes.calls != 3 {
				t.Fatalf("probe calls=%d, want 3", probes.calls)
			}
			if _, ok := wrapped.(uow.RevisionTransitionSource); !ok {
				t.Fatal("policy wrapper hides transition role")
			}
			if _, ok := wrapped.(uow.SourceSnapshotReaderSource); !ok {
				t.Fatal("policy wrapper hides atomic snapshot role")
			}
		})
	}
}

type admissionPatchUseCase struct {
	domain.IssueUseCase
	calls int
	err   error
}

func (u *admissionPatchUseCase) TransitionPatch(context.Context, storage.RevisionTransitionPatchPlan) (publicops.RevisionTransitionPatchResult, bool, error) {
	u.calls++
	return publicops.RevisionTransitionPatchResult{}, false, u.err
}
func (u *admissionPatchUseCase) GetRevisionTransitionPatchReceipt(context.Context, string) (*publicops.RevisionTransitionPatchReceipt, error) {
	u.calls++
	return nil, u.err
}

func TestAdmissionPatchSurvivesPolicyUseCase(t *testing.T) {
	want := errors.New("exact transaction failure")
	inner := &admissionPatchUseCase{err: want}
	wrapped := any(&issueUseCase{IssueUseCase: inner})
	patcher, ok := wrapped.(domain.RevisionTransitionPatchUseCase)
	if !ok {
		t.Fatal("policy wrapper hides atomic patch use case")
	}
	if _, _, err := patcher.TransitionPatch(context.Background(), storage.RevisionTransitionPatchPlan{}); !errors.Is(err, want) {
		t.Fatalf("patch error=%v", err)
	}
	if _, err := patcher.GetRevisionTransitionPatchReceipt(context.Background(), "receipt"); !errors.Is(err, want) {
		t.Fatalf("receipt error=%v", err)
	}
	if inner.calls != 2 {
		t.Fatalf("inner calls=%d, want 2", inner.calls)
	}
}
