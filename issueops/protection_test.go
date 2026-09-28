package issueops

import "testing"

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
