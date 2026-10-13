package httpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/steveyegge/beads/internal/types"
)

const sourceSnapshotPath = "/v0/beads/issues/"

// SourceSnapshot contains the durable issue detail and its complete outgoing
// durable dependency list from one server-side database snapshot. Issue.RowVersion
// is populated from the exact canonical string in Issue.Revision.
type SourceSnapshot struct {
	Issue                types.IssueDetails
	OutgoingDependencies []*types.Dependency
}

// GetSourceSnapshot reads one durable issue row and its outgoing dependency
// edges from the same transaction. It verifies project, database and
// capability identity before making the read, and rejects incomplete or
// mismatched response shapes rather than treating missing edges as empty.
func (c *Client) GetSourceSnapshot(ctx context.Context, issueID string) (*SourceSnapshot, error) {
	if c == nil {
		return nil, errors.New("httpclient: nil client")
	}
	if err := validateSourceSnapshotIssueID(issueID); err != nil {
		return nil, err
	}
	if err := c.verifyConfiguredSourceSnapshot(ctx); err != nil {
		return nil, err
	}
	response, err := c.do(ctx, http.MethodGet, sourceSnapshotPath+url.PathEscape(issueID)+"/sourceSnapshot", nil)
	if err != nil {
		return nil, err
	}
	return decodeSourceSnapshot(response, issueID)
}

// VerifySourceSnapshot checks that the authenticated endpoint serves the exact
// expected project and database and advertises both the snapshot operation and
// project-stamp enforcement. Reads repeat this handshake themselves.
func (c *Client) VerifySourceSnapshot(ctx context.Context, expectedProjectID, expectedDatabase string) error {
	if c == nil {
		return errors.New("httpclient: nil client")
	}
	if strings.TrimSpace(expectedProjectID) == "" {
		return errors.New("httpclient: expected project ID is required")
	}
	if expectedProjectID != c.projectID {
		return fmt.Errorf("httpclient: expected project ID %q does not match the configured request stamp %q", expectedProjectID, c.projectID)
	}
	if strings.TrimSpace(expectedDatabase) == "" {
		return errors.New("httpclient: expected database identity is required")
	}
	if expectedDatabase != c.database {
		return fmt.Errorf("httpclient: expected database %q does not match the configured database %q", expectedDatabase, c.database)
	}
	return c.verifyConfiguredSourceSnapshot(ctx)
}

func (c *Client) verifyConfiguredSourceSnapshot(ctx context.Context) error {
	server, err := c.GetContext(ctx)
	if err != nil {
		return err
	}
	if server.ProjectID != c.projectID {
		return fmt.Errorf("httpclient: server project ID %q does not match configured project %q", server.ProjectID, c.projectID)
	}
	if server.Database != c.database {
		return fmt.Errorf("httpclient: server database %q does not match configured database %q", server.Database, c.database)
	}
	if server.APIVersion != "v0" {
		return fmt.Errorf("httpclient: unsupported API version %q", server.APIVersion)
	}
	capabilities := make(map[string]struct{}, len(server.Capabilities))
	for _, capability := range server.Capabilities {
		capabilities[capability] = struct{}{}
	}
	for _, required := range []string{"issues.sourceSnapshot", "project.enforce"} {
		if _, ok := capabilities[required]; !ok {
			return fmt.Errorf("httpclient: server does not advertise required capability %q", required)
		}
	}
	return nil
}

func decodeSourceSnapshot(data []byte, requestedID string) (*SourceSnapshot, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return nil, errors.New("httpclient: source snapshot must be a JSON object")
	}
	issueRaw, ok := object["issue"]
	if !ok || len(bytes.TrimSpace(issueRaw)) == 0 || bytes.Equal(bytes.TrimSpace(issueRaw), []byte("null")) {
		return nil, errors.New("httpclient: source snapshot is missing issue")
	}
	edgesRaw, ok := object["outgoing_dependencies"]
	trimmedEdges := bytes.TrimSpace(edgesRaw)
	if !ok || len(trimmedEdges) == 0 || bytes.Equal(trimmedEdges, []byte("null")) || trimmedEdges[0] != '[' {
		return nil, errors.New("httpclient: source snapshot is missing its outgoing dependency array")
	}

	var issue types.IssueDetails
	if err := json.Unmarshal(issueRaw, &issue); err != nil {
		return nil, fmt.Errorf("httpclient: decode source snapshot issue: %w", err)
	}
	if issue.ID != requestedID {
		return nil, errors.New("httpclient: source snapshot issue ID does not match the requested ID")
	}
	version, err := types.ParseRevisionToken(issue.Revision)
	if err != nil || version == 0 || types.RevisionToken(version) != issue.Revision {
		return nil, errors.New("httpclient: source snapshot revision is not a nonzero canonical signed 64-bit decimal string")
	}
	issue.RowVersion = version

	var edges []*types.Dependency
	if err := json.Unmarshal(trimmedEdges, &edges); err != nil {
		return nil, fmt.Errorf("httpclient: decode outgoing dependencies: %w", err)
	}
	if edges == nil {
		return nil, errors.New("httpclient: source snapshot dependency list is null")
	}
	for i, edge := range edges {
		if edge == nil || edge.IssueID != requestedID || edge.DependsOnID == "" || edge.Type == "" {
			return nil, fmt.Errorf("httpclient: invalid outgoing dependency at index %d", i)
		}
	}
	return &SourceSnapshot{Issue: issue, OutgoingDependencies: edges}, nil
}

func validateSourceSnapshotIssueID(id string) error {
	if id == "" || !utf8.ValidString(id) || types.CheckFieldLen("id", id) != nil || hasControl(id) {
		return errors.New("httpclient: issue ID is empty or invalid")
	}
	return nil
}
