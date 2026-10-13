package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/steveyegge/beads/internal/httpapi/apigen"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
)

// handleGetSourceSnapshot serves the durable row and its complete outgoing
// durable edge list from the source-snapshot role's one database snapshot.
func (s *Server) handleGetSourceSnapshot(w http.ResponseWriter, r *http.Request) {
	if !s.sourceSnapshotReady {
		s.fail(w, r, SourceSnapshotNotImplemented())
		return
	}
	if !s.requireNoQuery(w, r) {
		return
	}
	id := r.PathValue("id")
	if id == "" || types.CheckFieldLen("id", id) != nil || strings.ContainsFunc(id, isControlChar) {
		s.fail(w, r, NotFound())
		return
	}
	reader, err := s.sourceSnapshotReader(r)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	snapshot, err := reader.ReadSourceSnapshot(r.Context(), id)
	if err != nil {
		var unsupported *storage.ErrUnsupported
		if errors.As(err, &unsupported) {
			s.fail(w, r, UnsupportedResource())
			return
		}
		s.failErr(w, r, err)
		return
	}
	if snapshot == nil || snapshot.Issue == nil || snapshot.Issue.ID != id {
		s.failErr(w, r, errors.New("source snapshot: role returned no matching durable issue row"))
		return
	}
	details := types.NewIssueDetails(*snapshot.Issue)
	details.Labels = append([]string(nil), snapshot.Issue.Labels...)
	edges := make([]apigen.Dependency, 0, len(snapshot.OutgoingDependencies))
	for _, edge := range snapshot.OutgoingDependencies {
		edges = append(edges, *edge)
	}
	writeJSON(w, apigen.SourceSnapshotResponse{
		Issue:                *details,
		OutgoingDependencies: edges,
	})
}
