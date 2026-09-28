package httpapi

import (
	"context"
	"net/http"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/steveyegge/beads/internal/storage/schema"
	"github.com/steveyegge/beads/issueops"
)

func TestBatchApplyReceiptReadinessGatesRequestAndCapability(t *testing.T) {
	applier := &roleBatchApplier{result: issueops.ApplyBatchResult{Keys: map[string]string{}, Items: []issueops.ItemResult{}}}
	server := newApplyBatchServer(t, applier)
	request := `{"actor":"alice","receipt_id":"retry-1","items":[{"kind":"create","create":{"title":"one"}}]}`

	resp := server.claim(t, batchApplyPath, request)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("receipt without readiness status = %d, want 501: %s", resp.StatusCode, readAll(t, resp))
	}
	problem := decodeBody(t, resp)
	if problem["code"] != string(CodeNotImplemented) {
		t.Fatalf("receipt readiness problem = %v, want code not_implemented", problem)
	}
	if got := len(applier.requests()); got != 0 {
		t.Fatalf("unready receipt request reached the batch role %d times, want 0", got)
	}
	contextBody := decodeBody(t, server.do(t, http.MethodGet, "/v0/beads/context", nil))
	capabilities, ok := contextBody["capabilities"].([]any)
	if !ok || hasString(capabilities, CapBatchApplyReceipt) {
		t.Fatalf("unready context advertised %q: %v", CapBatchApplyReceipt, contextBody["capabilities"])
	}
}

func TestBatchApplyReceiptCapabilityAndNormalAuthentication(t *testing.T) {
	applier := &roleBatchApplier{result: issueops.ApplyBatchResult{
		Keys: map[string]string{}, Items: []issueops.ItemResult{}, Replayed: true,
	}}
	server := newTestServer(t, rolesConfig(Config{
		Auth:                       authTokenFile(t, "token"),
		BatchApplier:               applier,
		BatchApplyReceiptReadiness: readyBatchApplyReceiptReadiness(t),
	}))
	contextBody := decodeBody(t, server.do(t, http.MethodGet, "/v0/beads/context", bearer("token")))
	capabilities, ok := contextBody["capabilities"].([]any)
	if !ok || !hasString(capabilities, CapBatchApplyReceipt) {
		t.Fatalf("ready context omitted %q: %v", CapBatchApplyReceipt, contextBody["capabilities"])
	}

	body := `{"actor":"alice","receipt_id":"retry-1","items":[{"kind":"create","create":{"title":"one"}}]}`
	unauthenticated := server.doBody(t, http.MethodPost, batchApplyPath, http.Header{}, "application/json", body)
	if unauthenticated.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated receipt replay status = %d, want 401: %s", unauthenticated.StatusCode, readAll(t, unauthenticated))
	}
	if got := len(applier.requests()); got != 0 {
		t.Fatalf("unauthenticated replay reached the batch role %d times, want 0", got)
	}

	resp := server.doBody(t, http.MethodPost, batchApplyPath, bearer("token"), "application/json", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated receipt replay status = %d, want 200: %s", resp.StatusCode, readAll(t, resp))
	}
	if got := decodeBody(t, resp)["replayed"]; got != true {
		t.Fatalf("response replayed = %v, want true", got)
	}
	requests := applier.requests()
	if len(requests) != 1 || requests[0].ReceiptID != "retry-1" {
		t.Fatalf("authorized request did not reach role with receipt ID: %#v", requests)
	}
}

func TestBatchApplyReceiptConflictHasStableProblem(t *testing.T) {
	applier := &roleBatchApplier{err: issueops.ErrBatchApplyReceiptConflict}
	server := newTestServer(t, rolesConfig(Config{
		BatchApplier:               applier,
		BatchApplyReceiptReadiness: readyBatchApplyReceiptReadiness(t),
	}))
	resp := server.claim(t, batchApplyPath,
		`{"actor":"alice","receipt_id":"retry-1","items":[{"kind":"create","create":{"title":"one"}}]}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("receipt conflict status = %d, want 409: %s", resp.StatusCode, readAll(t, resp))
	}
	problem := decodeBody(t, resp)
	if problem["code"] != string(CodeBatchApplyReceiptConflict) {
		t.Fatalf("receipt conflict code = %v, want %q", problem["code"], CodeBatchApplyReceiptConflict)
	}
	if problem["detail"] != "this receipt ID is already bound to a different request" {
		t.Fatalf("receipt conflict detail = %v, want the fixed detail", problem["detail"])
	}
	if problem["param"] != "receipt_id" {
		t.Fatalf("receipt conflict param = %v, want receipt_id", problem["param"])
	}
}

func readyBatchApplyReceiptReadiness(t *testing.T) schema.BatchApplyReceiptReadiness {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create batch receipt readiness DB: %v", err)
	}
	defer db.Close()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?")).
		WithArgs("schema_migrations").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(MAX(version), 0) FROM schema_migrations")).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(schema.BatchApplyReceiptMainMigration))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT receipt_id, request_digest, result_version, result_blob\n\t\tFROM issue_batch_apply_receipts LIMIT 0")).
		WillReturnRows(sqlmock.NewRows([]string{"receipt_id", "request_digest", "result_version", "result_blob"}))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT column_name FROM information_schema.statistics
		WHERE table_schema = DATABASE() AND table_name = ? AND index_name = 'PRIMARY'
		ORDER BY seq_in_index`)).WithArgs("issue_batch_apply_receipts").
		WillReturnRows(sqlmock.NewRows([]string{"column_name"}).AddRow("receipt_id"))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT column_name, data_type, character_maximum_length, is_nullable
		FROM information_schema.columns
		WHERE table_schema = DATABASE() AND table_name = 'issue_batch_apply_receipts'
		AND column_name IN ('receipt_id', 'request_digest', 'result_version', 'result_blob')`)).
		WillReturnRows(sqlmock.NewRows([]string{"column_name", "data_type", "character_maximum_length", "is_nullable"}).
			AddRow("receipt_id", "varbinary", int64(191), "NO").
			AddRow("request_digest", "char", int64(64), "NO").
			AddRow("result_version", "smallint", nil, "NO").
			AddRow("result_blob", "longblob", nil, "NO"))
	readiness, err := schema.ProbeBatchApplyReceiptReadiness(context.Background(), db)
	if err != nil {
		t.Fatalf("probe batch apply receipt readiness: %v", err)
	}
	if !readiness.Ready() {
		t.Fatal("complete batch receipt readiness proof was not ready")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("batch receipt readiness probe missed a prerequisite: %v", err)
	}
	return readiness
}
