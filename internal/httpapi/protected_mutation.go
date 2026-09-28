package httpapi

import (
	"errors"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/steveyegge/beads/internal/storage/schema"
	"github.com/steveyegge/beads/issueops"
)

const maxProtectedMutationPermitBytes = 64 << 10

// ProtectedMutationAuthority is immutable server-startup configuration for
// verifying protected-mutation permits. Its fields are private, and the server copies the
// value during Listen; request data and role credentials cannot replace it.
// The zero value disables protected mutation verification.
type ProtectedMutationAuthority struct {
	verifier  issueops.ProtectedMutationVerifier
	audience  string
	readiness schema.ProtectedMutationReadiness
}

// NewProtectedMutationAuthority constructs trusted startup configuration. The
// readiness value must come from ProbeProtectedMutationReadiness, and its
// verifier and audience cannot be changed after the value is constructed.
func NewProtectedMutationAuthority(
	verifier issueops.ProtectedMutationVerifier,
	audience string,
	readiness schema.ProtectedMutationReadiness,
) (ProtectedMutationAuthority, error) {
	if isNilProtectedMutationVerifier(verifier) || !readiness.Ready() ||
		audience == "" || strings.TrimSpace(audience) != audience || len(audience) > 255 || !utf8.ValidString(audience) {
		return ProtectedMutationAuthority{}, errors.New("httpapi: protected mutation authority requires a verifier, audience, and ready schema")
	}
	for _, r := range audience {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return ProtectedMutationAuthority{}, errors.New("httpapi: protected mutation audience contains control text")
		}
	}
	return ProtectedMutationAuthority{verifier: verifier, audience: audience, readiness: readiness}, nil
}

func (a ProtectedMutationAuthority) ready() bool {
	return !isNilProtectedMutationVerifier(a.verifier) && a.audience != "" && a.readiness.Ready()
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
