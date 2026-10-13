package uow

import (
	"context"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/schema"
)

// ProtectedMutationReadinessSource reports whether this provider's selected
// database has the schema required for protected mutation enforcement.
type ProtectedMutationReadinessSource interface {
	ProtectedMutationReadiness(context.Context) (schema.ProtectedMutationReadiness, error)
}

var _ ProtectedMutationReadinessSource = (*doltSQLProvider)(nil)
var _ ProtectedMutationReadinessSource = (*notifyingProvider)(nil)

// ProtectedMutationReadiness probes the selected database for protected
// mutation enforcement prerequisites. A probe error leaves readiness unknown.
func (p *doltSQLProvider) ProtectedMutationReadiness(ctx context.Context) (schema.ProtectedMutationReadiness, error) {
	if p == nil || p.db == nil {
		return schema.ProtectedMutationReadiness{}, &storage.ErrUnsupported{Op: "ProtectedMutationReadiness", Backend: "nil"}
	}
	return schema.ProbeProtectedMutationReadiness(ctx, p.db)
}

// ProtectedMutationReadiness forwards the probe to the selected provider.
func (p *notifyingProvider) ProtectedMutationReadiness(ctx context.Context) (schema.ProtectedMutationReadiness, error) {
	source, ok := p.inner.(ProtectedMutationReadinessSource)
	if !ok {
		return schema.ProtectedMutationReadiness{}, &storage.ErrUnsupported{Op: "ProtectedMutationReadiness", Backend: "provider"}
	}
	return source.ProtectedMutationReadiness(ctx)
}
