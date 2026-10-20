package telemetry

import (
	"context"

	"github.com/rhar1511/beads/internal/storage"
	"github.com/rhar1511/beads/issueops"
)

// SourceSnapshotReader preserves instrumentation around the atomic source
// snapshot role.
func (s *InstrumentedStorage) SourceSnapshotReader() (issueops.SourceSnapshotReader, error) {
	source, ok := s.inner.(interface {
		SourceSnapshotReader() (issueops.SourceSnapshotReader, error)
	})
	if !ok {
		return nil, &storage.ErrUnsupported{Op: "SourceSnapshotReader", Backend: "instrumented store"}
	}
	inner, err := source.SourceSnapshotReader()
	if err != nil {
		return nil, err
	}
	return &instrumentedSourceSnapshotReader{storage: s, inner: inner}, nil
}

type instrumentedSourceSnapshotReader struct {
	storage *InstrumentedStorage
	inner   issueops.SourceSnapshotReader
}

func (r *instrumentedSourceSnapshotReader) ReadSourceSnapshot(ctx context.Context, id string) (result *issueops.SourceSnapshot, err error) {
	ctx, span, started := r.storage.op(ctx, "SourceSnapshotReader.ReadSourceSnapshot")
	result, err = r.inner.ReadSourceSnapshot(ctx, id)
	r.storage.done(ctx, span, started, err)
	return result, err
}
