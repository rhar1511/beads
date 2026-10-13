package storage

import "github.com/steveyegge/beads/issueops"

// SourceSnapshotReader passes the read-only snapshot role through the hook
// decorator. A read emits no completion hook.
func (h *HookFiringStore) SourceSnapshotReader() (issueops.SourceSnapshotReader, error) {
	source, ok := h.inner.(interface {
		SourceSnapshotReader() (issueops.SourceSnapshotReader, error)
	})
	if !ok {
		return nil, &ErrUnsupported{Op: "SourceSnapshotReader", Backend: "hook store"}
	}
	return source.SourceSnapshotReader()
}
