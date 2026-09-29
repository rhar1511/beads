package issueops

import (
	"encoding/json"
	"testing"
)

func TestProtectedMutationDigestClearsPermitAndBindsRequest(t *testing.T) {
	base := UpdateRequest{
		Actor:   "caller",
		IssueID: "protected-1",
		Patch: IssuePatch{
			Title: Field[string]{Set: true, Value: "one"},
		},
		ProtectedPermit: "permit-a",
	}
	first, err := ProtectedMutationDigest(base)
	if err != nil {
		t.Fatalf("digest first request: %v", err)
	}

	replayedPermit := base
	replayedPermit.ProtectedPermit = "permit-b"
	second, err := ProtectedMutationDigest(replayedPermit)
	if err != nil {
		t.Fatalf("digest changed permit: %v", err)
	}
	if first != second {
		t.Fatalf("digest changed with permit alone: %s != %s", first, second)
	}

	changedField := base
	changedField.Patch.Title.Value = "two"
	third, err := ProtectedMutationDigest(changedField)
	if err != nil {
		t.Fatalf("digest changed field: %v", err)
	}
	if first == third {
		t.Fatal("digest did not change when a bound request field changed")
	}

	changedEndpoint := base
	changedEndpoint.Patch.ParentID = Field[string]{Set: true, Value: "protected-parent"}
	fourth, err := ProtectedMutationDigest(changedEndpoint)
	if err != nil {
		t.Fatalf("digest changed endpoint: %v", err)
	}
	if first == fourth {
		t.Fatal("digest did not change when a bound link endpoint changed")
	}

	changedID := base
	changedID.IssueID = "protected-2"
	fifth, err := ProtectedMutationDigest(changedID)
	if err != nil {
		t.Fatalf("digest changed issue ID: %v", err)
	}
	if first == fifth {
		t.Fatal("digest did not change when a bound issue ID changed")
	}
}

func TestApplyBatchRequestDigestBindsPersistedHiddenCreateInputs(t *testing.T) {
	request := ApplyBatchRequest{
		Actor:           "writer",
		ProtectedPermit: "permit-one",
		ReceiptID:       "receipt-one",
		Items: []ApplyItem{{
			Kind: ItemCreate,
			Create: &CreateItem{Issue: &Issue{
				Title:      "one",
				SourceRepo: "repo-one",
			}},
		}},
	}
	first, err := ApplyBatchRequestDigest(request)
	if err != nil {
		t.Fatalf("ApplyBatchRequestDigest: %v", err)
	}

	retry := request
	retry.ProtectedPermit = "permit-two"
	retry.ReceiptID = "receipt-two"
	second, err := ApplyBatchRequestDigest(retry)
	if err != nil {
		t.Fatalf("ApplyBatchRequestDigest retry: %v", err)
	}
	if first != second {
		t.Fatalf("permit or receipt identity changed digest: %q != %q", first, second)
	}

	changed := request
	changed.Items = append([]ApplyItem(nil), request.Items...)
	changed.Items[0].Create = &CreateItem{Issue: &Issue{Title: "one", SourceRepo: "repo-two"}}
	third, err := ApplyBatchRequestDigest(changed)
	if err != nil {
		t.Fatalf("ApplyBatchRequestDigest changed SourceRepo: %v", err)
	}
	if first == third {
		t.Fatal("persisted SourceRepo change did not change digest")
	}

	derived := request
	derived.Items = append([]ApplyItem(nil), request.Items...)
	derived.Items[0].Create = &CreateItem{Issue: &Issue{Title: "one", SourceRepo: "repo-one", RowVersion: 42}}
	fourth, err := ApplyBatchRequestDigest(derived)
	if err != nil {
		t.Fatalf("ApplyBatchRequestDigest derived field: %v", err)
	}
	if first != fourth {
		t.Fatalf("derived RowVersion changed digest: %q != %q", first, fourth)
	}
}

func TestProtectedCreateAndLinkDigestGolden(t *testing.T) {
	request := ApplyBatchRequest{
		Actor:           "controller-a",
		ProtectedPermit: "excluded-permit",
		ReceiptID:       "excluded-receipt",
		Items: []ApplyItem{
			{
				Kind: ItemCreate,
				Create: &CreateItem{
					Issue:           &Issue{ID: "bd-protected-1", Title: "Protected record"},
					ProtectionClass: "source-decision",
				},
			},
			{
				Kind: ItemDepAdd,
				DepAdd: &DepAddItem{
					Source: Ref{ID: "bd-protected-1"},
					Target: Ref{ID: "bd-map"},
					Type:   "relates-to",
				},
			},
			{
				Kind: ItemDepAdd,
				DepAdd: &DepAddItem{
					Source: Ref{ID: "bd-prerequisite"},
					Target: Ref{ID: "bd-protected-1"},
					Type:   "blocks",
				},
			},
		},
	}

	unsigned := request
	unsigned.ReceiptID = ""
	unsigned.ProtectedPermit = ""
	createSourceRepos := make([]string, len(unsigned.Items))
	encoded, err := json.Marshal(struct {
		Request           ApplyBatchRequest
		CreateSourceRepos []string
	}{Request: unsigned, CreateSourceRepos: createSourceRepos})
	if err != nil {
		t.Fatalf("marshal golden digest input: %v", err)
	}
	digest, err := ApplyBatchRequestDigest(request)
	if err != nil {
		t.Fatalf("ApplyBatchRequestDigest: %v", err)
	}

	const wantBytes = `{"Request":{"Actor":"controller-a","ProtectedPermit":"","Items":[{"Kind":"create","Create":{"Key":"","Issue":{"id":"bd-protected-1","title":"Protected record","priority":0,"created_at":"0001-01-01T00:00:00Z","updated_at":"0001-01-01T00:00:00Z"},"ProtectionClass":"source-decision","MetadataRefs":null},"Update":null,"Close":null,"DepAdd":null},{"Kind":"dep_add","Create":null,"Update":null,"Close":null,"DepAdd":{"Source":{"Key":"","ID":"bd-protected-1"},"Target":{"Key":"","ID":"bd-map"},"Type":"relates-to","Metadata":""}},{"Kind":"dep_add","Create":null,"Update":null,"Close":null,"DepAdd":{"Source":{"Key":"","ID":"bd-prerequisite"},"Target":{"Key":"","ID":"bd-protected-1"},"Type":"blocks","Metadata":""}}],"Provenance":"","ForceIDPrefix":false,"SkipPerEdgeCycleCheck":false},"CreateSourceRepos":["","",""]}`
	const wantDigest = "1a17901ea4ab71a9c88ad6981d904ad84860a3e9f8c0d0a00dbaee814fd88514"
	if string(encoded) != wantBytes || digest != wantDigest {
		t.Fatalf("Beads protected batch golden changed:\nbytes=%s\ndigest=%s", encoded, digest)
	}
}

func TestProtectedFrontierRecordDigestsMatchControllerGoldens(t *testing.T) {
	create := ApplyItem{
		Kind: ItemCreate,
		Create: &CreateItem{
			Issue: &Issue{
				ID:          "bd-frontier-map-1",
				Title:       "Decision map for wrk-1",
				Description: `{"schema_version":1,"kind":"decision-frontier/map/v1"}`,
				IssueType:   "gate",
				Metadata:    json.RawMessage(`{"gc.decision_frontier.record":"decision-frontier/map/v1","gc.decision_frontier.state":"pending"}`),
				Labels:      []string{"decision-frontier"},
			},
			ProtectionClass: "source-decision",
		},
	}
	linked := []ApplyItem{
		create,
		{
			Kind: ItemDepAdd,
			DepAdd: &DepAddItem{
				Source: Ref{ID: "bd-frontier-map-1"},
				Target: Ref{ID: "bd-question-1"},
				Type:   "relates-to",
			},
		},
		{
			Kind: ItemDepAdd,
			DepAdd: &DepAddItem{
				Source: Ref{ID: "bd-prerequisite"},
				Target: Ref{ID: "bd-frontier-map-1"},
				Type:   "blocks",
			},
		},
	}
	linkOnly := []ApplyItem{{
		Kind: ItemDepAdd,
		DepAdd: &DepAddItem{
			Source: Ref{ID: "bd-question-b"},
			Target: Ref{ID: "bd-question-a"},
			Type:   "blocks",
		},
	}}
	for _, test := range []struct {
		name   string
		items  []ApplyItem
		digest string
	}{
		{name: "create only", items: []ApplyItem{create}, digest: "f9bdd7a3fd99c83991c5288e92bb09da9b71ecc222809ac9faaded09ad796c85"},
		{name: "create with ordered links", items: linked, digest: "f05bb472b3b4f5a797586c3d6df1a2afec9e6a8f065f03e48f42b3d966bbf568"},
		{name: "link only", items: linkOnly, digest: "a1613932a1e5cf482be9f1d79e0cbd274cc21923987e016811324a406678306d"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := ApplyBatchRequestDigest(ApplyBatchRequest{
				Actor: "controller-a", ProtectedPermit: "excluded-permit",
				ReceiptID: "excluded-receipt", Items: test.items,
			})
			if err != nil {
				t.Fatalf("ApplyBatchRequestDigest: %v", err)
			}
			if got != test.digest {
				t.Fatalf("frontier record digest = %q, want controller golden %q", got, test.digest)
			}
		})
	}
}
