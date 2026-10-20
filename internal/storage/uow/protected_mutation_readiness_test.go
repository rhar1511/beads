package uow

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/rhar1511/beads/internal/storage/schema"
)

func TestProtectedMutationReadinessProbesSelectedProviderDatabase(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New(): %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?`)).
		WithArgs("schema_migrations").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	readiness, err := (&doltSQLProvider{db: db}).ProtectedMutationReadiness(context.Background())
	if err != nil {
		t.Fatalf("ProtectedMutationReadiness() error = %v", err)
	}
	if readiness.Ready() {
		t.Fatal("missing protected-mutation migration reported ready")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

type protectedMutationReadinessForwardingProvider struct {
	UnitOfWorkProvider
	readiness schema.ProtectedMutationReadiness
	err       error
	gotCtx    context.Context
}

type protectedMutationReadinessContextKey int

func (p *protectedMutationReadinessForwardingProvider) ProtectedMutationReadiness(ctx context.Context) (schema.ProtectedMutationReadiness, error) {
	p.gotCtx = ctx
	return p.readiness, p.err
}

func TestNotifyingProviderForwardsProtectedMutationReadiness(t *testing.T) {
	ctx := context.WithValue(context.Background(), protectedMutationReadinessContextKey(0), "readiness")
	wantErr := errors.New("probe failed")
	inner := &protectedMutationReadinessForwardingProvider{err: wantErr}
	provider := &notifyingProvider{inner: inner}

	readiness, err := provider.ProtectedMutationReadiness(ctx)
	if !errors.Is(err, wantErr) {
		t.Fatalf("ProtectedMutationReadiness() error = %v, want %v", err, wantErr)
	}
	if readiness.Ready() {
		t.Fatal("zero readiness proof reported ready after forwarding probe error")
	}
	if inner.gotCtx != ctx {
		t.Fatal("ProtectedMutationReadiness() did not forward the caller context")
	}
}
