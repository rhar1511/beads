package storage

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	publicops "github.com/steveyegge/beads/issueops"
)

func TestPlanRevisionTransitionPatchCanonicalizesAndCopiesPatch(t *testing.T) {
	metadataB := json.RawMessage(`{"z":2,"a":1}`)
	metadataA := json.RawMessage(`"old"`)
	request := publicops.RevisionTransitionPatchRequest{
		ReceiptID: "patch-r1", IssueID: "bd-1", Scope: "workspace-a", Kind: "route",
		Actor: "controller", ExpectedVersion: 17,
		PriorReceiptID: "prior-r1", PriorReceiptDigest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Patch: publicops.RevisionTransitionIssuePatch{
			Metadata: []publicops.RevisionTransitionMetadataPatch{
				{Key: "gc_z", Expected: &metadataB, Value: &metadataA},
				{Key: "gc_a", Value: &metadataB},
			},
			Status:   &publicops.RevisionTransitionStringPatch{Expected: "open", Value: "in_progress"},
			Assignee: &publicops.RevisionTransitionStringPatch{Expected: "", Value: "worker-a"},
			Labels:   &publicops.RevisionTransitionLabelsPatch{Expected: []string{"z", "a"}, Value: []string{"new", "a"}},
		},
	}

	plan, err := PlanRevisionTransitionPatch(request)
	if err != nil {
		t.Fatalf("PlanRevisionTransitionPatch: %v", err)
	}
	if got := []string{plan.Patch.Metadata[0].Key, plan.Patch.Metadata[1].Key}; !reflect.DeepEqual(got, []string{"gc_a", "gc_z"}) {
		t.Fatalf("metadata keys = %v, want canonical sort", got)
	}
	if string(*plan.Patch.Metadata[1].Expected) != `{"a":1,"z":2}` {
		t.Fatalf("canonical metadata expected = %s", *plan.Patch.Metadata[1].Expected)
	}
	if got := plan.Patch.Labels.Expected; !reflect.DeepEqual(got, []string{"a", "z"}) {
		t.Fatalf("expected labels = %v, want sorted labels", got)
	}
	if got := plan.Patch.Labels.Value; !reflect.DeepEqual(got, []string{"a", "new"}) {
		t.Fatalf("next labels = %v, want sorted labels", got)
	}
	// Planning owns a snapshot: caller mutation after validation cannot alter
	// the request the transaction or receipt will bind.
	metadataA[0] = 'x'
	request.Patch.Metadata[0].Key = "changed"
	if plan.Patch.Metadata[1].Key != "gc_z" || string(*plan.Patch.Metadata[1].Value) != `"old"` {
		t.Fatalf("plan changed after caller mutation: %+v", plan.Patch.Metadata)
	}
}

func TestPlanRevisionTransitionPatchRejectsInvalidIdentityAndPreconditions(t *testing.T) {
	base := publicops.RevisionTransitionPatchRequest{
		ReceiptID: "patch-r1", IssueID: "bd-1", Scope: "workspace-a", Kind: "route",
		Actor: "controller", ExpectedVersion: 17,
		PriorReceiptID: "prior-r1", PriorReceiptDigest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Patch: publicops.RevisionTransitionIssuePatch{
			Metadata: []publicops.RevisionTransitionMetadataPatch{{Key: "gc_a", Value: rawPatchValue(`1`)}},
		},
	}
	tests := []struct {
		name   string
		mutate func(*publicops.RevisionTransitionPatchRequest)
	}{
		{name: "missing parent", mutate: func(r *publicops.RevisionTransitionPatchRequest) { r.PriorReceiptID = "" }},
		{name: "parent exceeds byte key bound", mutate: func(r *publicops.RevisionTransitionPatchRequest) { r.PriorReceiptID = strings.Repeat("é", 96) }},
		{name: "invalid parent digest", mutate: func(r *publicops.RevisionTransitionPatchRequest) { r.PriorReceiptDigest = "not-a-digest" }},
		{name: "empty patch", mutate: func(r *publicops.RevisionTransitionPatchRequest) { r.Patch = publicops.RevisionTransitionIssuePatch{} }},
		{name: "duplicate metadata keys", mutate: func(r *publicops.RevisionTransitionPatchRequest) {
			r.Patch.Metadata = append(r.Patch.Metadata, publicops.RevisionTransitionMetadataPatch{Key: "gc_a", Value: rawPatchValue(`2`)})
		}},
		{name: "unchanged metadata key", mutate: func(r *publicops.RevisionTransitionPatchRequest) {
			r.Patch.Metadata[0].Expected = rawPatchValue(`1`)
		}},
		{name: "invalid metadata JSON", mutate: func(r *publicops.RevisionTransitionPatchRequest) {
			bad := json.RawMessage(`{`)
			r.Patch.Metadata[0].Value = &bad
		}},
		{name: "unchanged status", mutate: func(r *publicops.RevisionTransitionPatchRequest) {
			r.Patch.Status = &publicops.RevisionTransitionStringPatch{Expected: "open", Value: "open"}
		}},
		{name: "duplicate labels", mutate: func(r *publicops.RevisionTransitionPatchRequest) {
			r.Patch.Labels = &publicops.RevisionTransitionLabelsPatch{Expected: []string{"a", "a"}, Value: []string{"b"}}
		}},
		{name: "unchanged labels", mutate: func(r *publicops.RevisionTransitionPatchRequest) {
			r.Patch.Labels = &publicops.RevisionTransitionLabelsPatch{Expected: []string{"a", "b"}, Value: []string{"b", "a"}}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := base
			request.Patch.Metadata = append([]publicops.RevisionTransitionMetadataPatch(nil), base.Patch.Metadata...)
			tt.mutate(&request)
			if _, err := PlanRevisionTransitionPatch(request); err == nil {
				t.Fatal("PlanRevisionTransitionPatch error = nil, want validation failure")
			}
		})
	}
}

func rawPatchValue(value string) *json.RawMessage {
	raw := json.RawMessage(value)
	return &raw
}
