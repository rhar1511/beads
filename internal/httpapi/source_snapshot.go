package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/steveyegge/beads/internal/storage/uow"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/issueops"
)

// handleSourceSnapshot reads the source row and its exact outgoing edge records
// through one role call. The role, rather than this handler, owns the read
// transaction so a caller cannot observe a row from one version of the graph
// and edges from another.
func (s *Server) handleSourceSnapshot(w http.ResponseWriter, r *http.Request) {
	if !s.acceptQuery(w, r, newQuery(r.URL.Query())) {
		return
	}
	id := r.PathValue("id")
	if id == "" || types.CheckFieldLen("id", id) != nil || strings.ContainsFunc(id, isControlChar) {
		s.fail(w, r, NotFound())
		return
	}
	var reader issueops.SourceSnapshotReader
	if s.provider != nil {
		var err error
		reader, err = uow.NewSourceSnapshotReader(timedProvider{inner: s.provider, rec: requestInfo(r.Context())})
		if err != nil {
			s.failErr(w, r, err)
			return
		}
	} else {
		reader = s.issueSourceSnapshot
	}
	if reader == nil {
		s.fail(w, r, newResult(CodeDBUnavailable, "source snapshots are unavailable on this storage backend"))
		return
	}
	snapshot, err := reader.ReadSourceSnapshot(r.Context(), id)
	if err != nil {
		s.failReadErr(w, r, err)
		return
	}
	if snapshot.Issue == nil || snapshot.Issue.ID != id {
		s.failErr(w, r, errors.New("source snapshot returned no matching issue"))
		return
	}
	writeJSON(w, struct {
		Issue struct {
			*types.Issue
			Revision string `json:"revision"`
		} `json:"issue"`
		OutgoingDependencies []*types.Dependency `json:"outgoing_dependencies"`
	}{Issue: struct {
		*types.Issue
		Revision string `json:"revision"`
	}{snapshot.Issue, types.RevisionToken(snapshot.Issue.RowVersion)}, OutgoingDependencies: snapshot.OutgoingDependencies})
}
