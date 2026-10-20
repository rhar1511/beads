//go:build cgo

package embeddeddolt

import (
	"context"
	"database/sql"

	"github.com/rhar1511/beads/internal/storage"
	storageissueops "github.com/rhar1511/beads/internal/storage/issueops"
	publicops "github.com/rhar1511/beads/issueops"
)

// SourceSnapshotReader returns the atomic durable source read for this store.
func (s *EmbeddedDoltStore) SourceSnapshotReader() (publicops.SourceSnapshotReader, error) {
	if s == nil {
		return nil, &storage.ErrUnsupported{Op: "SourceSnapshotReader", Backend: "nil"}
	}
	return &sourceSnapshotReader{store: s}, nil
}

type sourceSnapshotReader struct{ store *EmbeddedDoltStore }

var _ publicops.SourceSnapshotReader = (*sourceSnapshotReader)(nil)

func (r *sourceSnapshotReader) ReadSourceSnapshot(ctx context.Context, id string) (*publicops.SourceSnapshot, error) {
	var snapshot *publicops.SourceSnapshot
	err := r.store.withConn(ctx, false, func(tx *sql.Tx) error {
		var err error
		snapshot, err = storageissueops.ExecuteDurableSourceSnapshotInTx(ctx, tx, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return snapshot, nil
}
