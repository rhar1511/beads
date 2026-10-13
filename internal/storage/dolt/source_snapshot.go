package dolt

import (
	"context"
	"database/sql"

	"github.com/steveyegge/beads/internal/storage"
	storageissueops "github.com/steveyegge/beads/internal/storage/issueops"
	publicops "github.com/steveyegge/beads/issueops"
)

// SourceSnapshotReader returns the atomic durable source read for this store.
func (s *DoltStore) SourceSnapshotReader() (publicops.SourceSnapshotReader, error) {
	if s == nil {
		return nil, &storage.ErrUnsupported{Op: "SourceSnapshotReader", Backend: "nil"}
	}
	return &sourceSnapshotReader{store: s}, nil
}

type sourceSnapshotReader struct{ store *DoltStore }

var _ publicops.SourceSnapshotReader = (*sourceSnapshotReader)(nil)

func (r *sourceSnapshotReader) ReadSourceSnapshot(ctx context.Context, id string) (*publicops.SourceSnapshot, error) {
	var snapshot *publicops.SourceSnapshot
	err := r.store.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		snapshot, err = storageissueops.ExecuteDurableSourceSnapshotInTx(ctx, tx, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return snapshot, nil
}
