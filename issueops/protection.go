package issueops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
)

// ProtectedMutationPermit is an opaque signed permit issued by a caller's
// trusted authorization service. Beads deliberately does not define key
// management or a policy vocabulary.
type ProtectedMutationPermit string

// ProtectedMutationScope is the exact operation a permit may authorize.
// ResourceIDs contains canonical issue IDs and both ends of every link. Digest
// binds the canonical request body when the caller can calculate one.
type ProtectedMutationScope struct {
	Audience    string
	Operation   string
	ResourceIDs []string
	Digest      string
}

// ProtectedMutationVerification is the verifier's result after checking the
// permit signature, expiry, audience, and exact scope. ReplayID must be the
// permit's stable, issuer-assigned replay identity. Beads consumes that
// identity in its own transaction.
type ProtectedMutationVerification struct {
	ReplayID string
}

// ProtectedMutationVerifier verifies the permit signature, expiry, exact
// scope, and replay identity. Beads inserts the returned replay identity into
// its unique-use table in the same transaction as the protected write.
// Implementations own their key and issuer policy; ordinary Beads roles do not
// receive transaction access from this seam.
type ProtectedMutationVerifier interface {
	Verify(ctx context.Context, permit ProtectedMutationPermit, scope ProtectedMutationScope) (ProtectedMutationVerification, error)
}

// ProtectedMutationDigest returns a deterministic SHA-256 digest of a request
// value encoded as JSON. If the request has a ProtectedPermit field, the
// function hashes a copy with that field cleared. encoding/json sorts map
// keys, so metadata and patch maps have stable output.
func ProtectedMutationDigest(value any) (string, error) {
	value = clearProtectedPermit(value)
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode protected mutation request: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

// ApplyBatchRequestDigest returns the canonical identity digest used by durable
// batchApply receipts. It excludes the retry identity itself and the opaque
// protected permit, while binding every operation and request option.
func ApplyBatchRequestDigest(request ApplyBatchRequest) (string, error) {
	request.ReceiptID = ""
	request.ProtectedPermit = ""
	// Issue.SourceRepo is execution input for create items: the public create
	// projection copies it into the persisted row. Generic Issue JSON omits it
	// because it is local routing state, so carry it beside the ordinary JSON
	// request. The other json:"-" Issue fields are derived output or are ignored
	// by publicCreateIssue and therefore do not belong to request identity.
	createSourceRepos := make([]string, len(request.Items))
	for index, item := range request.Items {
		if item.Create != nil && item.Create.Issue != nil {
			createSourceRepos[index] = item.Create.Issue.SourceRepo
		}
	}
	return ProtectedMutationDigest(struct {
		Request           ApplyBatchRequest
		CreateSourceRepos []string
	}{Request: request, CreateSourceRepos: createSourceRepos})
}

func clearProtectedPermit(value any) any {
	rv := reflect.ValueOf(value)
	if !rv.IsValid() {
		return value
	}
	wasPointer := false
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return value
		}
		wasPointer = true
		copy := reflect.New(rv.Elem().Type())
		copy.Elem().Set(rv.Elem())
		rv = copy.Elem()
	} else if rv.Kind() == reflect.Struct {
		copy := reflect.New(rv.Type())
		copy.Elem().Set(rv)
		rv = copy.Elem()
	} else {
		return value
	}
	field := rv.FieldByName("ProtectedPermit")
	if field.IsValid() && field.CanSet() && field.Type() == reflect.TypeOf(ProtectedMutationPermit("")) {
		field.Set(reflect.Zero(field.Type()))
	}
	if wasPointer {
		return rv.Addr().Interface()
	}
	return rv.Interface()
}
