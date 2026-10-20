package httpapi

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/rhar1511/beads/internal/storage/domain"
	"github.com/rhar1511/beads/internal/storage/schema"
	"github.com/rhar1511/beads/issueops"
)

type testProtectedMutationVerifier struct{}

func (testProtectedMutationVerifier) Verify(context.Context, issueops.ProtectedMutationPermit, issueops.ProtectedMutationScope) (issueops.ProtectedMutationVerification, error) {
	return issueops.ProtectedMutationVerification{ReplayID: "test-replay"}, nil
}

func TestProtectedMutationAuthorityRequiresVerifiedSchema(t *testing.T) {
	if _, err := NewProtectedMutationAuthority(testProtectedMutationVerifier{}, "audience-a", "project-a", "database-a", schema.ProtectedMutationReadiness{}); err == nil {
		t.Fatal("authority accepted zero-value schema readiness")
	}
	if _, err := NewProtectedMutationAuthority(nil, "audience-a", "project-a", "database-a", readyProtectedMutationReadiness(t)); err == nil {
		t.Fatal("authority accepted a nil verifier")
	}
}

func TestProtectedMutationCapabilityAndPermitRequireStartupAuthority(t *testing.T) {
	transitionReadiness := readyTransitionReadinessProof(t)
	ordinaryRole := &revisionTransitionTestRole{result: issueops.RevisionTransitionResult{Applied: true}}
	ordinary := newTestServer(t, rolesConfig(Config{
		Auth: authTokenFile(t, "token"), RevisionTransition: ordinaryRole,
		TransitionReadiness: transitionReadiness,
		Workspace:           domain.ContextInfo{ProjectID: "project-a", Database: "database-a"},
	}))
	ordinaryContext := decodeBody(t, ordinary.do(t, http.MethodGet, "/v0/beads/context", bearer("token")))
	ordinaryCapabilities, ok := ordinaryContext["capabilities"].([]any)
	if !ok || hasString(ordinaryCapabilities, CapProtectedMutation) {
		t.Fatalf("context without protected authority advertised protected mutation: %v", ordinaryContext["capabilities"])
	}

	readiness := readyProtectedMutationReadiness(t)
	authority, err := NewProtectedMutationAuthority(testProtectedMutationVerifier{}, "audience-a", "project-a", "database-a", readiness)
	if err != nil {
		t.Fatalf("construct authority: %v", err)
	}
	unready := newTestServer(t, rolesConfig(Config{
		Auth: authTokenFile(t, "token"), RevisionTransition: &revisionTransitionTestRole{},
		ProtectedMutationAuthority: authority,
		Workspace:                  domain.ContextInfo{ProjectID: "project-a", Database: "database-a"},
	}))
	if unready.protectedMutationReady {
		t.Fatal("verifier and replay schema bypassed the Q43 transition readiness gate")
	}
	unreadyContext := decodeBody(t, unready.do(t, http.MethodGet, "/v0/beads/context", bearer("token")))
	unreadyCapabilities, ok := unreadyContext["capabilities"].([]any)
	if !ok || hasString(unreadyCapabilities, CapProtectedMutation) {
		t.Fatalf("context without Q43 readiness advertised protected mutation: %v", unreadyContext["capabilities"])
	}

	role := &revisionTransitionTestRole{results: []issueops.RevisionTransitionResult{
		{Applied: true},
		{Applied: true, Replayed: true},
	}}
	server := newTestServer(t, rolesConfig(Config{
		Auth: authTokenFile(t, "token"), RevisionTransition: role,
		TransitionReadiness: transitionReadiness, ProtectedMutationAuthority: authority,
		Workspace: domain.ContextInfo{ProjectID: "project-a", Database: "database-a"},
	}))
	if !server.revisionTransitionReady || !server.protectedMutationReady {
		t.Fatal("Listen did not enable protected transitions with ready verifier and replay schema")
	}
	contextBody := decodeBody(t, server.do(t, http.MethodGet, "/v0/beads/context", bearer("token")))
	capabilities, ok := contextBody["capabilities"].([]any)
	if !ok || !hasString(capabilities, CapProtectedMutation) {
		t.Fatalf("ready context omitted protected mutation capability: %v", contextBody["capabilities"])
	}

	const permit = "opaque-signed-permit"
	headers := bearer("token")
	headers.Set(ProjectIDHeader, "project-a")
	body := transitionBodyWithProtectedPermit(permit)
	first := server.doBody(t, http.MethodPost, "/v0/beads/issues/bd-1:transitionMetadata", headers, "application/json", body)
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first transition status = %d, want 200: %s", first.StatusCode, readAll(t, first))
	}
	firstResponse := decodeBody(t, first)
	if firstResponse["applied"] != true || firstResponse["replayed"] != false {
		t.Fatalf("first transition result = %v, want applied once", firstResponse)
	}
	second := server.doBody(t, http.MethodPost, "/v0/beads/issues/bd-1:transitionMetadata", headers, "application/json", body)
	if second.StatusCode != http.StatusOK {
		t.Fatalf("replayed transition status = %d, want 200: %s", second.StatusCode, readAll(t, second))
	}
	secondResponse := decodeBody(t, second)
	if secondResponse["applied"] != true || secondResponse["replayed"] != true {
		t.Fatalf("replayed transition result = %v, want exact replay", secondResponse)
	}
	requests := role.recorded()
	if len(requests) != 2 || requests[0].ProtectedPermit != permit || requests[1].ProtectedPermit != permit {
		t.Fatalf("role requests did not preserve the opaque permit on both attempts: %#v", requests)
	}
	if requests[0].ReceiptID != requests[1].ReceiptID || requests[0].IssueID != requests[1].IssueID {
		t.Fatalf("repeated request changed resource/receipt binding: %#v", requests)
	}
	if got := role.protectedAuthorityRecorded(); len(got) != 2 || !got[0] || !got[1] {
		t.Fatalf("role authority context = %v, want trusted startup authority on both calls", got)
	}
}

func TestProtectedMutationAuthorityMustMatchSelectedWorkspace(t *testing.T) {
	readiness := readyProtectedMutationReadiness(t)
	authority, err := NewProtectedMutationAuthority(testProtectedMutationVerifier{}, "audience-a", "project-a", "database-a", readiness)
	if err != nil {
		t.Fatalf("construct authority: %v", err)
	}
	_, err = Listen(Config{
		Addr: "127.0.0.1:0", Auth: authTokenFile(t, "token"),
		ProtectedMutationAuthority: authority,
		Workspace:                  domain.ContextInfo{ProjectID: "project-a", Database: "database-b"},
	})
	if err == nil || !strings.Contains(err.Error(), "does not match the selected project and database") {
		t.Fatalf("Listen error = %v, want selected-workspace binding refusal", err)
	}
}

func readyProtectedMutationReadiness(t *testing.T) schema.ProtectedMutationReadiness {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create protected readiness DB: %v", err)
	}
	defer db.Close()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?")).
		WithArgs("schema_migrations").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(MAX(version), 0) FROM schema_migrations")).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(schema.ProtectedMutationMainMigration))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT issue_id, protection_class FROM protected_issue_records LIMIT 0")).
		WillReturnRows(sqlmock.NewRows([]string{"issue_id", "protection_class"}))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT audience, replay_id FROM protected_mutation_permit_uses LIMIT 0")).
		WillReturnRows(sqlmock.NewRows([]string{"audience", "replay_id"}))
	primaryKeyQuery := regexp.QuoteMeta(`SELECT column_name FROM information_schema.statistics
		WHERE table_schema = DATABASE() AND table_name = ? AND index_name = 'PRIMARY'
		ORDER BY seq_in_index`)
	mock.ExpectQuery(primaryKeyQuery).WithArgs("protected_issue_records").
		WillReturnRows(sqlmock.NewRows([]string{"column_name"}).AddRow("issue_id"))
	mock.ExpectQuery(primaryKeyQuery).WithArgs("protected_mutation_permit_uses").
		WillReturnRows(sqlmock.NewRows([]string{"column_name"}).AddRow("audience").AddRow("replay_id"))
	columnQuery := regexp.QuoteMeta(`SELECT data_type, character_maximum_length, is_nullable
		FROM information_schema.columns
		WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?`)
	for _, column := range []struct {
		table  string
		name   string
		length int64
	}{
		{table: "protected_issue_records", name: "issue_id", length: 255},
		{table: "protected_issue_records", name: "protection_class", length: 64},
		{table: "protected_mutation_permit_uses", name: "audience", length: 255},
		{table: "protected_mutation_permit_uses", name: "replay_id", length: 255},
	} {
		mock.ExpectQuery(columnQuery).WithArgs(column.table, column.name).
			WillReturnRows(sqlmock.NewRows([]string{"data_type", "character_maximum_length", "is_nullable"}).
				AddRow("varchar", column.length, "NO"))
	}
	readiness, err := schema.ProbeProtectedMutationReadiness(context.Background(), db)
	if err != nil {
		t.Fatalf("probe protected readiness: %v", err)
	}
	if !readiness.Ready() {
		t.Fatal("complete protected readiness proof was not ready")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("protected readiness probe missed a prerequisite: %v", err)
	}
	return readiness
}
