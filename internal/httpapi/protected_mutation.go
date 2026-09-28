package httpapi

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/steveyegge/beads/internal/storage/schema"
	"github.com/steveyegge/beads/issueops"
)

const maxProtectedMutationPermitBytes = 64 << 10

// protectedMutationPermitMember reads an optional opaque permit without
// trimming or otherwise changing it. A permit is authorization input, so
// whitespace, invalid UTF-8, controls, and overlong values are rejected at
// the edge. Keep this validation shared by every HTTP mutation that carries a
// protected permit.
func protectedMutationPermitMember(raw map[string]json.RawMessage) (issueops.ProtectedMutationPermit, *Result) {
	encoded, present := raw["protected_permit"]
	if !present {
		return "", nil
	}
	if !utf8.Valid(encoded) {
		res := InvalidArgument("protected_permit", ReasonInvalidValue,
			"`protected_permit` must be nonempty UTF-8 text of at most 65536 bytes")
		return "", &res
	}
	var token *string
	if err := json.Unmarshal(encoded, &token); err != nil || token == nil || *token == "" ||
		len(*token) > maxProtectedMutationPermitBytes || !utf8.ValidString(*token) ||
		strings.TrimSpace(*token) != *token || hasProtectedMutationControl(*token) {
		res := InvalidArgument("protected_permit", ReasonInvalidValue,
			"`protected_permit` must be nonempty trimmed UTF-8 text of at most 65536 bytes without control characters")
		return "", &res
	}
	return issueops.ProtectedMutationPermit(*token), nil
}

func hasProtectedMutationControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) || r == '\u2028' || r == '\u2029' {
			return true
		}
	}
	return false
}

// ProtectedMutationAuthority is immutable server-startup configuration for
// verifying protected-mutation permits. Its fields are private, and the server copies the
// value during Listen; request data and role credentials cannot replace it.
// The zero value disables protected mutation verification.
type ProtectedMutationAuthority struct {
	verifier  issueops.ProtectedMutationVerifier
	audience  string
	projectID string
	database  string
	readiness schema.ProtectedMutationReadiness
}

// NewProtectedMutationAuthority constructs trusted startup configuration. The
// readiness value must come from ProbeProtectedMutationReadiness, and its
// verifier and audience cannot be changed after the value is constructed.
func NewProtectedMutationAuthority(
	verifier issueops.ProtectedMutationVerifier,
	audience string,
	projectID string,
	database string,
	readiness schema.ProtectedMutationReadiness,
) (ProtectedMutationAuthority, error) {
	if isNilProtectedMutationVerifier(verifier) || !readiness.Ready() ||
		!validProtectedBinding(audience, 255) || !validProtectedBinding(projectID, 255) || !validProtectedBinding(database, 255) {
		return ProtectedMutationAuthority{}, errors.New("httpapi: protected mutation authority requires a verifier, audience, project, database, and ready schema")
	}
	return ProtectedMutationAuthority{verifier: verifier, audience: audience, projectID: projectID, database: database, readiness: readiness}, nil
}

func (a ProtectedMutationAuthority) ready() bool {
	return !isNilProtectedMutationVerifier(a.verifier) && validProtectedBinding(a.audience, 255) &&
		validProtectedBinding(a.projectID, 255) && validProtectedBinding(a.database, 255) && a.readiness.Ready()
}

func (a ProtectedMutationAuthority) configured() bool {
	return !isNilProtectedMutationVerifier(a.verifier) || a.audience != "" || a.projectID != "" || a.database != "" || a.readiness.Ready()
}

func (a ProtectedMutationAuthority) matchesWorkspace(projectID, database string) bool {
	return a.projectID == projectID && a.database == database
}

func validProtectedBinding(value string, maxBytes int) bool {
	if value == "" || strings.TrimSpace(value) != value || len(value) > maxBytes || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return false
		}
	}
	return true
}

func isNilProtectedMutationVerifier(verifier issueops.ProtectedMutationVerifier) bool {
	if verifier == nil {
		return true
	}
	value := reflect.ValueOf(verifier)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
