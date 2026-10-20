package issueops

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-sql-driver/mysql"
	"github.com/rhar1511/beads/internal/storage"
	publicops "github.com/rhar1511/beads/issueops"
)

func TestRevisionTransitionReceiptInsertErrorClassification(t *testing.T) {
	duplicate := classifyRevisionTransitionReceiptInsertError("r-1", &mysql.MySQLError{Number: 1062, Message: "duplicate entry"})
	if !errors.Is(duplicate, ErrRevisionTransitionReceiptRace) {
		t.Fatalf("duplicate-key insert error = %v, want receipt race", duplicate)
	}

	insertFailure := errors.New("Error 1406 (22001): Data too long for column 'payload_json'")
	got := classifyRevisionTransitionReceiptInsertError("r-1", insertFailure)
	if errors.Is(got, ErrRevisionTransitionReceiptRace) {
		t.Fatalf("non-duplicate insert error = %v, must not be treated as a receipt race", got)
	}
	if !errors.Is(got, insertFailure) {
		t.Fatalf("non-duplicate insert error = %v, want original insert failure preserved", got)
	}
}

func TestPlanRevisionTransitionRejectsIncompleteRequest(t *testing.T) {
	value := json.RawMessage(`"next"`)
	expected := json.RawMessage(`"before"`)
	base := publicops.RevisionTransitionRequest{
		ReceiptID:       "r-1",
		IssueID:         "bd-1",
		Scope:           "frontier",
		Kind:            "ensure",
		Actor:           "controller",
		ExpectedVersion: 11,
		Key:             "gc_frontier_ticket",
		Expected:        &expected,
		Value:           &value,
		Payload:         json.RawMessage(`{"ticket":"t-1"}`),
	}
	tests := []struct {
		name   string
		mutate func(*publicops.RevisionTransitionRequest)
	}{
		{name: "empty receipt id", mutate: func(r *publicops.RevisionTransitionRequest) { r.ReceiptID = "" }},
		{name: "empty issue id", mutate: func(r *publicops.RevisionTransitionRequest) { r.IssueID = "" }},
		{name: "empty scope", mutate: func(r *publicops.RevisionTransitionRequest) { r.Scope = "" }},
		{name: "empty kind", mutate: func(r *publicops.RevisionTransitionRequest) { r.Kind = "" }},
		{name: "empty actor", mutate: func(r *publicops.RevisionTransitionRequest) { r.Actor = "" }},
		{name: "zero expected revision", mutate: func(r *publicops.RevisionTransitionRequest) { r.ExpectedVersion = 0 }},
		{name: "invalid metadata key", mutate: func(r *publicops.RevisionTransitionRequest) { r.Key = " " }},
		{name: "unchanged metadata marker", mutate: func(r *publicops.RevisionTransitionRequest) { same := json.RawMessage(`"before"`); r.Value = &same }},
		{name: "invalid payload JSON", mutate: func(r *publicops.RevisionTransitionRequest) { r.Payload = json.RawMessage(`{`) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := base
			tt.mutate(&req)
			if _, err := storage.PlanRevisionTransition(req); err == nil {
				t.Fatal("PlanRevisionTransition error = nil, want validation failure")
			}
		})
	}
}

func TestGetRevisionTransitionReceiptRejectsCorruptRows(t *testing.T) {
	tests := []struct {
		name            string
		from, to        int64
		expectedPresent int
		expectedJSON    any
		valuePresent    int
		valueJSON       any
		payload         string
		wantErr         bool
	}{
		{name: "valid", from: 11, to: 22, valuePresent: 1, valueJSON: `"next"`, payload: `{}`},
		{name: "zero from revision", from: 0, to: 22, valuePresent: 1, valueJSON: `"next"`, payload: `{}`, wantErr: true},
		{name: "same revision tokens", from: 11, to: 11, valuePresent: 1, valueJSON: `"next"`, payload: `{}`, wantErr: true},
		{name: "invalid presence flag", from: 11, to: 22, valuePresent: 2, valueJSON: `"next"`, payload: `{}`, wantErr: true},
		{name: "unexpected expected JSON", from: 11, to: 22, expectedJSON: `null`, valuePresent: 1, valueJSON: `"next"`, payload: `{}`, wantErr: true},
		{name: "noncanonical expected JSON", from: 11, to: 22, expectedPresent: 1, expectedJSON: ` 1 `, valuePresent: 1, valueJSON: `"next"`, payload: `{}`, wantErr: true},
		{name: "noncanonical value JSON", from: 11, to: 22, valuePresent: 1, valueJSON: ` "next" `, payload: `{}`, wantErr: true},
		{name: "same expected and next value", from: 11, to: 22, expectedPresent: 1, expectedJSON: `"same"`, valuePresent: 1, valueJSON: `"same"`, payload: `{}`, wantErr: true},
		{name: "noncanonical payload", from: 11, to: 22, valuePresent: 1, valueJSON: `"next"`, payload: `{ "a" : 1 }`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock.New: %v", err)
			}
			defer db.Close()
			mock.ExpectQuery(regexp.QuoteMeta("SELECT receipt_id, issue_id, scope, kind, actor, metadata_key,")).
				WillReturnRows(sqlmock.NewRows([]string{
					"receipt_id", "issue_id", "scope", "kind", "actor", "metadata_key",
					"from_revision", "to_revision", "expected_present", "expected_json",
					"value_present", "value_json", "payload_json",
				}).AddRow("r-1", "bd-1", "frontier", "ensure", "controller", "gc_frontier_ticket",
					tt.from, tt.to, tt.expectedPresent, tt.expectedJSON, tt.valuePresent, tt.valueJSON, tt.payload))

			_, err = getRevisionTransitionReceiptInTx(context.Background(), db, "r-1")
			if (err != nil) != tt.wantErr {
				t.Fatalf("getRevisionTransitionReceiptInTx error = %v, wantErr %v", err, tt.wantErr)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("unmet SQL expectations: %v", err)
			}
		})
	}
}
