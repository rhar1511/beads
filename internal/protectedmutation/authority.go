// Package protectedmutation verifies short-lived signed permits for exact
// Beads mutations. It does not classify records, execute mutations, or
// consume replay identities.
package protectedmutation

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/steveyegge/beads/issueops"
)

const (
	SchemaVersionV1            = "beads.protected-mutation-permit.v1"
	RevocationsSchemaVersionV1 = "beads.protected-mutation-revocations.v1"
	PermitKeyPurpose           = "protected_mutation_permit"
	RevocationKeyPurpose       = "protected_mutation_revocation"
	PermitSigningDomain        = "beads.protected-mutation-permit.v1\n"
	RevocationSigningDomain    = "beads.protected-mutation-revocations.v1\n"

	maxPermitPayloadBytes = 32 << 10
	maxPermitTokenBytes   = 64 << 10
	maxRevocationBytes    = 4 << 20
	maxTrustedKeys        = 64
	maxAudienceBytes      = 255
	maxResourceIDs        = 256
	maxRevokedReplayIDs   = 65536
	maxPermitAge          = 5 * time.Minute
	maxRevocationAge      = 5 * time.Minute
	clockSkew             = 30 * time.Second
)

var (
	ErrUnavailable  = errors.New("protected mutation authority unavailable")
	ErrMalformed    = errors.New("protected mutation authority data is malformed")
	ErrUnknownKey   = errors.New("protected mutation signing key is unknown")
	ErrBadSignature = errors.New("protected mutation signature is invalid")
	ErrPurpose      = errors.New("protected mutation signing purpose mismatch")
	ErrAuthority    = errors.New("protected mutation issuer or key mismatch")
	ErrAudience     = errors.New("protected mutation audience mismatch")
	ErrWorkspace    = errors.New("protected mutation project or database mismatch")
	ErrScope        = errors.New("protected mutation scope mismatch")
	ErrDigest       = errors.New("protected mutation request digest mismatch")
	ErrExpired      = errors.New("protected mutation authority data expired")
	ErrNotYetValid  = errors.New("protected mutation authority data is not yet valid")
	ErrRevoked      = errors.New("protected mutation permit is revoked")
	ErrGeneration   = errors.New("protected mutation authority generations differ")
)

// TrustedKey is one host-authorized Ed25519 key with one signing purpose.
// Keys for permits and revocation snapshots must be distinct.
type TrustedKey struct {
	KeyID     string `json:"key_id"`
	Issuer    string `json:"issuer"`
	Purpose   string `json:"purpose"`
	PublicKey string `json:"public_key"`
}

// Keyring is the host-mounted public-key set. Generation changes whenever
// key membership or purpose assignments change.
type Keyring struct {
	SchemaVersion int          `json:"schema_version"`
	Generation    uint64       `json:"generation"`
	Keys          []TrustedKey `json:"keys"`
}

// Claims authorizes one exact mutation. ResourceIDs are a set serialized in
// sorted order; ProjectID and Database are separate workspace claims.
type Claims struct {
	SchemaVersion string   `json:"schema_version"`
	Purpose       string   `json:"purpose"`
	KeyID         string   `json:"key_id"`
	Issuer        string   `json:"issuer"`
	Audience      string   `json:"audience"`
	ProjectID     string   `json:"project_id"`
	Database      string   `json:"database"`
	Operation     string   `json:"operation"`
	ResourceIDs   []string `json:"resource_ids"`
	RequestDigest string   `json:"request_digest"`
	ReplayID      string   `json:"replay_id"`
	IssuedAt      string   `json:"issued_at"`
	ExpiresAt     string   `json:"expires_at"`
}

// Revocations is one current signed snapshot for an audience and exact
// project/database workspace. Its generation must match the host keyring.
type Revocations struct {
	SchemaVersion    string   `json:"schema_version"`
	Purpose          string   `json:"purpose"`
	KeyID            string   `json:"key_id"`
	Issuer           string   `json:"issuer"`
	Audience         string   `json:"audience"`
	ProjectID        string   `json:"project_id"`
	Database         string   `json:"database"`
	Generation       uint64   `json:"generation"`
	IssuedAt         string   `json:"issued_at"`
	ExpiresAt        string   `json:"expires_at"`
	RevokedReplayIDs []string `json:"revoked_replay_ids"`
}

// SignedRevocations carries canonical payload bytes and a raw-URL-base64
// Ed25519 signature over RevocationSigningDomain followed by those bytes.
type SignedRevocations struct {
	Payload   json.RawMessage `json:"payload"`
	Signature string          `json:"signature"`
}

// Bundle is the current host-mounted trust snapshot.
type Bundle struct {
	Generation  uint64
	Keys        []TrustedKey
	Revocations SignedRevocations
}

// Source supplies current host-authorized keys and signed revocations. The
// verifier loads it on startup and again for every permit.
type Source interface {
	Load(context.Context) (Bundle, error)
}

// Options are trusted startup bindings captured by bd serve. Lifetime limits
// and clock skew are fixed in this package and cannot be widened by config.
type Options struct {
	Audience  string
	ProjectID string
	Database  string
	Now       func() time.Time
}

// Verifier authenticates permits against a current host-authority snapshot.
type Verifier struct {
	source  Source
	options Options
}

var _ issueops.ProtectedMutationVerifier = (*Verifier)(nil)

// NewVerifier validates the independent audience and project/database
// bindings. There is no default or inferred authority.
func NewVerifier(source Source, options Options) (*Verifier, error) {
	if source == nil || !validAtom(options.Audience, maxAudienceBytes) || !validAtom(options.ProjectID, 255) || !validAtom(options.Database, 255) {
		return nil, ErrUnavailable
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Verifier{source: source, options: options}, nil
}

// ValidateStartup verifies usable keys and the current signed revocation
// snapshot before bd serve opens its listener.
func (v *Verifier) ValidateStartup(ctx context.Context) error {
	if v == nil || v.source == nil {
		return ErrUnavailable
	}
	bundle, err := v.source.Load(ctx)
	if err != nil {
		return fmt.Errorf("load protected mutation authority: %w", err)
	}
	keys, err := validateKeys(bundle.Keys)
	if err != nil {
		return err
	}
	_, err = verifyRevocations(bundle.Revocations, bundle.Generation, keys, v.options, v.options.Now())
	return err
}

// Close closes a source that pins host files. Sources without close resources
// need no cleanup.
func (v *Verifier) Close() error {
	if v == nil {
		return nil
	}
	closer, ok := v.source.(interface{ Close() error })
	if !ok {
		return nil
	}
	return closer.Close()
}

// Verify checks the permit signature and every server-derived scope field.
// The returned replay ID is authenticated and is consumed by Beads in the
// same transaction as the protected write.
func (v *Verifier) Verify(ctx context.Context, permit issueops.ProtectedMutationPermit, scope issueops.ProtectedMutationScope) (issueops.ProtectedMutationVerification, error) {
	if v == nil || v.source == nil {
		return issueops.ProtectedMutationVerification{}, ErrUnavailable
	}
	want, err := validateExpectation(scope, v.options)
	if err != nil {
		return issueops.ProtectedMutationVerification{}, err
	}
	claims, payload, signature, err := decodePermit(string(permit))
	if err != nil {
		return issueops.ProtectedMutationVerification{}, err
	}
	bundle, err := v.source.Load(ctx)
	if err != nil {
		return issueops.ProtectedMutationVerification{}, fmt.Errorf("load protected mutation authority: %w", err)
	}
	keys, err := validateKeys(bundle.Keys)
	if err != nil {
		return issueops.ProtectedMutationVerification{}, err
	}
	revoked, err := verifyRevocations(bundle.Revocations, bundle.Generation, keys, v.options, v.options.Now())
	if err != nil {
		return issueops.ProtectedMutationVerification{}, err
	}
	if claims.Purpose != PermitKeyPurpose {
		return issueops.ProtectedMutationVerification{}, ErrPurpose
	}
	key, ok := keys[claims.KeyID]
	if !ok {
		return issueops.ProtectedMutationVerification{}, ErrUnknownKey
	}
	if key.purpose != PermitKeyPurpose {
		return issueops.ProtectedMutationVerification{}, ErrPurpose
	}
	if key.issuer != claims.Issuer {
		return issueops.ProtectedMutationVerification{}, ErrAuthority
	}
	if !ed25519.Verify(key.public, append([]byte(PermitSigningDomain), payload...), signature) {
		return issueops.ProtectedMutationVerification{}, ErrBadSignature
	}
	if _, _, err := validateFreshness(claims.IssuedAt, claims.ExpiresAt, maxPermitAge, v.options.Now()); err != nil {
		return issueops.ProtectedMutationVerification{}, err
	}
	if claims.Audience != v.options.Audience || claims.Audience != want.Audience {
		return issueops.ProtectedMutationVerification{}, ErrAudience
	}
	if claims.ProjectID != v.options.ProjectID || claims.Database != v.options.Database ||
		claims.ProjectID != want.ProjectID || claims.Database != want.Database {
		return issueops.ProtectedMutationVerification{}, ErrWorkspace
	}
	if claims.Operation != want.Operation || !equalStrings(claims.ResourceIDs, want.ResourceIDs) {
		return issueops.ProtectedMutationVerification{}, ErrScope
	}
	if claims.RequestDigest != want.RequestDigest {
		return issueops.ProtectedMutationVerification{}, ErrDigest
	}
	if _, found := revoked[claims.ReplayID]; found {
		return issueops.ProtectedMutationVerification{}, ErrRevoked
	}
	return issueops.ProtectedMutationVerification{ReplayID: claims.ReplayID}, nil
}

// GrantSigningBytes returns the dedicated signing domain plus the canonical
// permit payload, suitable for independent permit issuers.
func GrantSigningBytes(claims Claims) ([]byte, error) {
	payload, err := canonicalClaims(claims)
	if err != nil {
		return nil, err
	}
	return append([]byte(PermitSigningDomain), payload...), nil
}

// RevocationSigningBytes returns the dedicated signing domain plus the
// canonical revocation payload.
func RevocationSigningBytes(list Revocations) ([]byte, error) {
	payload, err := canonicalRevocations(list)
	if err != nil {
		return nil, err
	}
	return append([]byte(RevocationSigningDomain), payload...), nil
}

type verifiedKey struct {
	issuer  string
	purpose string
	public  ed25519.PublicKey
}

func validateKeys(entries []TrustedKey) (map[string]verifiedKey, error) {
	if len(entries) == 0 || len(entries) > maxTrustedKeys {
		return nil, ErrUnavailable
	}
	keys := make(map[string]verifiedKey, len(entries))
	material := make(map[[sha256.Size]byte]string, len(entries))
	hasPermitKey, hasRevocationKey := false, false
	for _, entry := range entries {
		if !validAtom(entry.KeyID, 200) || !validAtom(entry.Issuer, 256) ||
			(entry.Purpose != PermitKeyPurpose && entry.Purpose != RevocationKeyPurpose) {
			return nil, ErrMalformed
		}
		if _, duplicate := keys[entry.KeyID]; duplicate {
			return nil, ErrMalformed
		}
		raw, err := base64.StdEncoding.DecodeString(entry.PublicKey)
		if err != nil || len(raw) != ed25519.PublicKeySize || base64.StdEncoding.EncodeToString(raw) != entry.PublicKey {
			return nil, ErrMalformed
		}
		fingerprint := sha256.Sum256(raw)
		if _, duplicate := material[fingerprint]; duplicate {
			return nil, ErrMalformed
		}
		material[fingerprint] = entry.KeyID
		keys[entry.KeyID] = verifiedKey{issuer: entry.Issuer, purpose: entry.Purpose, public: append(ed25519.PublicKey(nil), raw...)}
		hasPermitKey = hasPermitKey || entry.Purpose == PermitKeyPurpose
		hasRevocationKey = hasRevocationKey || entry.Purpose == RevocationKeyPurpose
	}
	if !hasPermitKey || !hasRevocationKey {
		return nil, ErrUnavailable
	}
	return keys, nil
}

func verifyRevocations(envelope SignedRevocations, generation uint64, keys map[string]verifiedKey, options Options, now time.Time) (map[string]struct{}, error) {
	if len(envelope.Payload) == 0 || len(envelope.Payload) > maxRevocationBytes {
		return nil, ErrUnavailable
	}
	var list Revocations
	if err := decodeStrictJSON(envelope.Payload, &list); err != nil {
		return nil, err
	}
	canonical, err := canonicalRevocations(list)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(canonical, envelope.Payload) {
		return nil, ErrMalformed
	}
	key, ok := keys[list.KeyID]
	if !ok {
		return nil, ErrUnknownKey
	}
	if list.Purpose != RevocationKeyPurpose || key.purpose != RevocationKeyPurpose {
		return nil, ErrPurpose
	}
	if key.issuer != list.Issuer {
		return nil, ErrAuthority
	}
	signature, err := base64.RawURLEncoding.DecodeString(envelope.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize || base64.RawURLEncoding.EncodeToString(signature) != envelope.Signature {
		return nil, ErrMalformed
	}
	if !ed25519.Verify(key.public, append([]byte(RevocationSigningDomain), envelope.Payload...), signature) {
		return nil, ErrBadSignature
	}
	if list.Generation != generation {
		return nil, ErrGeneration
	}
	if list.Audience != options.Audience {
		return nil, ErrAudience
	}
	if list.ProjectID != options.ProjectID || list.Database != options.Database {
		return nil, ErrWorkspace
	}
	if _, _, err := validateFreshness(list.IssuedAt, list.ExpiresAt, maxRevocationAge, now); err != nil {
		return nil, err
	}
	revoked := make(map[string]struct{}, len(list.RevokedReplayIDs))
	for _, replayID := range list.RevokedReplayIDs {
		revoked[replayID] = struct{}{}
	}
	return revoked, nil
}

type expectation struct {
	Audience      string
	ProjectID     string
	Database      string
	Operation     string
	ResourceIDs   []string
	RequestDigest string
}

func validateExpectation(scope issueops.ProtectedMutationScope, options Options) (expectation, error) {
	if !validAtom(scope.Audience, maxAudienceBytes) || !validAtom(scope.ProjectID, 255) || !validAtom(scope.Database, 255) ||
		!validAtom(scope.Operation, 200) || !validSHA256(scope.Digest) {
		return expectation{}, ErrMalformed
	}
	if scope.Audience != options.Audience {
		return expectation{}, ErrAudience
	}
	if scope.ProjectID != options.ProjectID || scope.Database != options.Database {
		return expectation{}, ErrWorkspace
	}
	resources, err := canonicalIDs(scope.ResourceIDs, maxResourceIDs, true)
	if err != nil {
		return expectation{}, err
	}
	return expectation{
		Audience: scope.Audience, ProjectID: scope.ProjectID, Database: scope.Database,
		Operation: scope.Operation, ResourceIDs: resources, RequestDigest: scope.Digest,
	}, nil
}

func decodePermit(token string) (Claims, []byte, []byte, error) {
	if token == "" || len(token) > maxPermitTokenBytes || strings.TrimSpace(token) != token {
		return Claims{}, nil, nil, ErrMalformed
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return Claims{}, nil, nil, ErrMalformed
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || base64.RawURLEncoding.EncodeToString(payload) != parts[0] || len(payload) > maxPermitPayloadBytes {
		return Claims{}, nil, nil, ErrMalformed
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(signature) != ed25519.SignatureSize || base64.RawURLEncoding.EncodeToString(signature) != parts[1] {
		return Claims{}, nil, nil, ErrMalformed
	}
	var claims Claims
	if err := decodeStrictJSON(payload, &claims); err != nil {
		return Claims{}, nil, nil, err
	}
	canonical, err := canonicalClaims(claims)
	if err != nil {
		return Claims{}, nil, nil, err
	}
	if !bytes.Equal(canonical, payload) {
		return Claims{}, nil, nil, ErrMalformed
	}
	return claims, payload, signature, nil
}

func canonicalClaims(claims Claims) ([]byte, error) {
	if claims.SchemaVersion != SchemaVersionV1 {
		return nil, ErrMalformed
	}
	if claims.Purpose != PermitKeyPurpose {
		return nil, ErrPurpose
	}
	if !validAtom(claims.KeyID, 200) || !validAtom(claims.Issuer, 256) || !validAtom(claims.Audience, maxAudienceBytes) ||
		!validAtom(claims.ProjectID, 255) || !validAtom(claims.Database, 255) || !validAtom(claims.Operation, 200) ||
		!validSHA256(claims.RequestDigest) || !validReplayID(claims.ReplayID) {
		return nil, ErrMalformed
	}
	resources, err := canonicalIDs(claims.ResourceIDs, maxResourceIDs, true)
	if err != nil {
		return nil, err
	}
	if _, err := parseCanonicalTime(claims.IssuedAt); err != nil {
		return nil, err
	}
	if _, err := parseCanonicalTime(claims.ExpiresAt); err != nil {
		return nil, err
	}
	claims.ResourceIDs = resources
	payload, err := json.Marshal(claims)
	if err != nil || len(payload) > maxPermitPayloadBytes {
		return nil, ErrMalformed
	}
	return payload, nil
}

func canonicalRevocations(list Revocations) ([]byte, error) {
	if list.SchemaVersion != RevocationsSchemaVersionV1 {
		return nil, ErrMalformed
	}
	if list.Purpose != RevocationKeyPurpose {
		return nil, ErrPurpose
	}
	if !validAtom(list.KeyID, 200) || !validAtom(list.Issuer, 256) || !validAtom(list.Audience, maxAudienceBytes) ||
		!validAtom(list.ProjectID, 255) || !validAtom(list.Database, 255) || list.Generation == 0 {
		return nil, ErrMalformed
	}
	if list.RevokedReplayIDs == nil {
		return nil, ErrMalformed
	}
	ids, err := canonicalIDs(list.RevokedReplayIDs, maxRevokedReplayIDs, false)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if !validReplayID(id) {
			return nil, ErrMalformed
		}
	}
	if _, err := parseCanonicalTime(list.IssuedAt); err != nil {
		return nil, err
	}
	if _, err := parseCanonicalTime(list.ExpiresAt); err != nil {
		return nil, err
	}
	list.RevokedReplayIDs = ids
	payload, err := json.Marshal(list)
	if err != nil || len(payload) > maxRevocationBytes {
		return nil, ErrMalformed
	}
	return payload, nil
}

func canonicalIDs(values []string, limit int, requireNonEmpty bool) ([]string, error) {
	if len(values) > limit || requireNonEmpty && len(values) == 0 {
		return nil, ErrMalformed
	}
	result := make([]string, len(values))
	copy(result, values)
	for _, value := range result {
		if !validAtom(value, 512) {
			return nil, ErrMalformed
		}
	}
	sort.Strings(result)
	for i := 1; i < len(result); i++ {
		if result[i] == result[i-1] {
			return nil, ErrMalformed
		}
	}
	return result, nil
}

func validateFreshness(issuedText, expiresText string, maxAge time.Duration, now time.Time) (time.Time, time.Time, error) {
	issued, err := parseCanonicalTime(issuedText)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	expires, err := parseCanonicalTime(expiresText)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if !expires.After(issued) || expires.Sub(issued) > maxAge || now.Sub(issued) > maxAge+clockSkew {
		return time.Time{}, time.Time{}, ErrExpired
	}
	if now.Add(clockSkew).Before(issued) {
		return time.Time{}, time.Time{}, ErrNotYetValid
	}
	if !now.Add(-clockSkew).Before(expires) {
		return time.Time{}, time.Time{}, ErrExpired
	}
	return issued, expires, nil
}

func parseCanonicalTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || parsed.Format(time.RFC3339Nano) != value {
		return time.Time{}, ErrMalformed
	}
	return parsed, nil
}

func decodeStrictJSON(raw []byte, destination any) error {
	if err := rejectDuplicateJSONMembers(raw); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return ErrMalformed
	}
	return nil
}

func rejectDuplicateJSONMembers(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := consumeJSONValue(decoder); err != nil {
		return fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return ErrMalformed
	}
	return nil
}

func consumeJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return ErrMalformed
			}
			if _, duplicate := seen[key]; duplicate {
				return ErrMalformed
			}
			seen[key] = struct{}{}
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return ErrMalformed
		}
	case '[':
		for decoder.More() {
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return ErrMalformed
		}
	default:
		return ErrMalformed
	}
	return nil
}

func validAtom(value string, maxBytes int) bool {
	if value == "" || len(value) > maxBytes || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validReplayID(value string) bool {
	if len(value) < 16 || len(value) > 200 {
		return false
	}
	for i, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case i > 0 && (r == '.' || r == '_' || r == ':' || r == '-'):
		default:
			return false
		}
	}
	return true
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
