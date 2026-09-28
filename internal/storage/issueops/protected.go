package issueops

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/steveyegge/beads/internal/storage"
	publicops "github.com/steveyegge/beads/issueops"
)

type protectedMutationConfig struct {
	mu               sync.Mutex
	verifier         publicops.ProtectedMutationVerifier
	audience         string
	verified         []publicops.ProtectedMutationScope
	permitUseWritten bool
}

type protectedMutationConfigKey struct{}

// WithProtectedMutationVerifier installs the backend's trusted verifier and
// audience for the lifetime of a request. The verifier validates the signed
// permit and returns its replay identity; Beads consumes that identity in the
// active SQL transaction. This package is internal so ordinary external role
// callers cannot replace the backend verifier.
func WithProtectedMutationVerifier(ctx context.Context, verifier publicops.ProtectedMutationVerifier, audience string) context.Context {
	return context.WithValue(ctx, protectedMutationConfigKey{}, &protectedMutationConfig{verifier: verifier, audience: strings.TrimSpace(audience)})
}

// ProtectedMutationAuthorityConfigured reports whether this request carries
// the backend-installed authority needed for protected writes. It lets the UOW
// adapter distinguish legacy in-memory test doubles, which cannot contain the
// storage-managed classification table, from an authority-bearing request
// that must fail closed if its transaction cannot enforce the check.
func ProtectedMutationAuthorityConfigured(ctx context.Context) bool {
	config, _ := ctx.Value(protectedMutationConfigKey{}).(*protectedMutationConfig)
	return config != nil && config.verifier != nil && strings.TrimSpace(config.audience) != ""
}

// ResetProtectedMutationAttempt clears per-transaction authorization cache
// state before a retried transaction body runs. A permit accepted by a rolled
// back attempt must be verified and consumed again in the replacement tx.
func ResetProtectedMutationAttempt(ctx context.Context) {
	if config, _ := ctx.Value(protectedMutationConfigKey{}).(*protectedMutationConfig); config != nil {
		config.mu.Lock()
		config.verified = nil
		config.permitUseWritten = false
		config.mu.Unlock()
	}
}

// ProtectedMutationPermitUseWritten reports whether this transaction consumed
// a replay identity, so the Dolt wrapper can stage that operational write with
// the protected mutation.
func ProtectedMutationPermitUseWritten(ctx context.Context) bool {
	if config, _ := ctx.Value(protectedMutationConfigKey{}).(*protectedMutationConfig); config != nil {
		config.mu.Lock()
		defer config.mu.Unlock()
		return config.permitUseWritten
	}
	return false
}

// RequireProtectedMutation checks the explicit classification of every named
// endpoint in the transaction and verifies a signed permit if the operation
// touches one, or if explicitClass is non-empty for a create/classification
// change. It does not infer classification from issue contents or receipts.
func RequireProtectedMutation(ctx context.Context, tx DBTX, permit publicops.ProtectedMutationPermit, operation string, resourceIDs []string, digest string, explicitClass string) error {
	ids := canonicalProtectedIDs(resourceIDs)
	protected, err := ProtectedRecordIDsInTx(ctx, tx, ids)
	if err != nil {
		return fmt.Errorf("check protected records: %w", err)
	}
	if len(protected) == 0 && explicitClass == "" {
		return nil
	}
	if explicitClass != "" && !validProtectionClass(explicitClass) {
		return fmt.Errorf("%w: invalid protection class", storage.ErrValidation)
	}
	config, _ := ctx.Value(protectedMutationConfigKey{}).(*protectedMutationConfig)
	if config != nil && protectedAuthorizationCached(config, operation, config.audience, ids, digest) {
		return nil
	}
	if config == nil || config.verifier == nil || config.audience == "" || permit == "" {
		return fmt.Errorf("%w: operation %s touches an explicitly classified record", publicops.ErrProtectedRecord, operation)
	}
	scope := publicops.ProtectedMutationScope{
		Audience:    config.audience,
		Operation:   operation,
		ResourceIDs: ids,
		Digest:      digest,
	}
	verified, err := config.verifier.Verify(ctx, permit, scope)
	if err != nil {
		return fmt.Errorf("%w: %v", publicops.ErrProtectedRecord, err)
	}
	if strings.TrimSpace(verified.ReplayID) != verified.ReplayID || verified.ReplayID == "" || len(verified.ReplayID) > 255 {
		return fmt.Errorf("%w: verifier returned an invalid replay identity", publicops.ErrProtectedRecord)
	}
	if err := ConsumeProtectedMutationPermitInTx(ctx, tx, config.audience, verified); err != nil {
		return fmt.Errorf("%w: consume permit replay identity: %v", publicops.ErrProtectedRecord, err)
	}
	config.mu.Lock()
	config.verified = append(config.verified, scope)
	config.permitUseWritten = true
	config.mu.Unlock()
	return nil
}

// ConsumeProtectedMutationPermitInTx claims the verifier-validated replay
// identity in the active mutation transaction. The identity is retained
// permanently so clock skew and cleanup races cannot make a valid replay pass.
func ConsumeProtectedMutationPermitInTx(ctx context.Context, tx DBTX, audience string, verified publicops.ProtectedMutationVerification) error {
	if strings.TrimSpace(audience) == "" || strings.TrimSpace(verified.ReplayID) != verified.ReplayID || verified.ReplayID == "" || len(verified.ReplayID) > 255 {
		return fmt.Errorf("invalid protected mutation replay identity")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO protected_mutation_permit_uses
		(audience, replay_id) VALUES (?, ?)`, audience, verified.ReplayID); err != nil {
		return fmt.Errorf("replay identity already consumed or unavailable: %w", err)
	}
	return nil
}

func protectedAuthorizationCached(config *protectedMutationConfig, operation, audience string, resourceIDs []string, digest string) bool {
	config.mu.Lock()
	defer config.mu.Unlock()
	for _, verified := range config.verified {
		if verified.Audience != audience {
			continue
		}
		if verified.Operation == operation && verified.Digest == digest && equalProtectedIDs(verified.ResourceIDs, resourceIDs) {
			return true
		}
		if !batchScopeCovers(verified.Operation, operation) || !protectedIDsSubset(resourceIDs, verified.ResourceIDs) {
			continue
		}
		return true
	}
	return false
}

// RefuseUnpermittedProtectedMutationInTx is the non-consuming guard for
// mutation surfaces that do not carry a permit. It also protects SQL helpers
// shared by typed roles: an already-verified outer operation may cover one of
// its explicit lower-level writes, but this function never verifies or
// consumes another replay identity.
func RefuseUnpermittedProtectedMutationInTx(ctx context.Context, tx DBTX, operation string, resourceIDs []string) error {
	ids := canonicalProtectedIDs(resourceIDs)
	protected, err := ProtectedRecordIDsInTx(ctx, tx, ids)
	if err != nil {
		return fmt.Errorf("check protected records before %s: %w", operation, err)
	}
	if len(protected) == 0 {
		return nil
	}
	config, _ := ctx.Value(protectedMutationConfigKey{}).(*protectedMutationConfig)
	if config != nil && protectedAuthorizationCovers(config, operation, protected) {
		return nil
	}
	return fmt.Errorf("%w: operation %s touches explicitly classified record(s): %s",
		publicops.ErrProtectedRecord, operation, strings.Join(protected, ", "))
}

// RefuseUnpermittedProtectedDerivedMutationInTx checks derived maintenance
// writes such as blocked-state recomputation. A protected row may be updated
// only when it is explicitly named by a permit already verified for this same
// transaction attempt; a permit for another row does not extend to neighbors.
func RefuseUnpermittedProtectedDerivedMutationInTx(ctx context.Context, tx DBTX, operation string, resourceIDs []string) error {
	ids := canonicalProtectedIDs(resourceIDs)
	protected, err := ProtectedRecordIDsInTx(ctx, tx, ids)
	if err != nil {
		return fmt.Errorf("check protected records before %s: %w", operation, err)
	}
	if len(protected) == 0 {
		return nil
	}
	config, _ := ctx.Value(protectedMutationConfigKey{}).(*protectedMutationConfig)
	if config != nil && protectedDerivedAuthorizationCovers(config, operation, protected) {
		return nil
	}
	return fmt.Errorf("%w: derived operation %s touches explicitly classified record(s): %s",
		publicops.ErrProtectedRecord, operation, strings.Join(protected, ", "))
}

func protectedAuthorizationCovers(config *protectedMutationConfig, operation string, resourceIDs []string) bool {
	config.mu.Lock()
	defer config.mu.Unlock()
	for _, verified := range config.verified {
		if verified.Operation != operation && !protectedOperationCovers(verified.Operation, operation) {
			continue
		}
		if protectedIDsSubset(resourceIDs, verified.ResourceIDs) {
			return true
		}
	}
	return false
}

func protectedDerivedAuthorizationCovers(config *protectedMutationConfig, operation string, resourceIDs []string) bool {
	config.mu.Lock()
	defer config.mu.Unlock()
	for _, verified := range config.verified {
		if protectedDerivedOperationCovers(verified.Operation, operation) && protectedIDsSubset(resourceIDs, verified.ResourceIDs) {
			return true
		}
	}
	return false
}

func protectedDerivedOperationCovers(outer, derived string) bool {
	if derived != "issue.derived_recompute" {
		return false
	}
	switch outer {
	case "issue.create", "issue.update", "issue.close", "issue.reopen",
		"issue.batch_apply", "issue.batch_create", "issue.batch_close",
		"dependency.add", "dependency.remove":
		return true
	default:
		return false
	}
}

// protectedOperationCovers names only the leaf writes that a typed request
// actually composes. Resource membership is checked separately against the
// complete outer request scope.
func protectedOperationCovers(outer, leaf string) bool {
	if batchScopeCovers(outer, leaf) {
		return true
	}
	switch outer {
	case "issue.create":
		return leaf == "dependency.add" || leaf == "issue.label_add" || leaf == "issue.comment"
	case "issue.batch_create":
		return leaf == "dependency.add" || leaf == "issue.label_add"
	case "issue.update":
		return leaf == "dependency.add" || leaf == "dependency.remove" || leaf == "issue.label_add" || leaf == "issue.label_remove" || leaf == "issue.claim" || leaf == "issue.persistence_move" || leaf == "issue.update"
	case "issue.reopen":
		return leaf == "issue.comment"
	case "metadata.compare_and_set":
		return leaf == "issue.update"
	default:
		return false
	}
}

func batchScopeCovers(batchOperation, operation string) bool {
	switch batchOperation {
	case "issue.batch_apply":
		return operation == "issue.create" || operation == "issue.update" || operation == "issue.close" || operation == "dependency.add" || operation == "issue.label_add" || operation == "issue.label_remove"
	case "issue.batch_create":
		return operation == "issue.create" || operation == "issue.label_add"
	case "issue.batch_close":
		return operation == "issue.close"
	default:
		return false
	}
}

func protectedIDsSubset(subset, superset []string) bool {
	allowed := make(map[string]struct{}, len(superset))
	for _, id := range superset {
		allowed[id] = struct{}{}
	}
	for _, id := range subset {
		if _, ok := allowed[id]; !ok {
			return false
		}
	}
	return true
}

func equalProtectedIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ProtectedRecordIDsInTx returns explicitly classified IDs among ids. The
// storage-managed classification table is shared by issue and wisp IDs; an
// empty classification is never treated as protected.
func ProtectedRecordIDsInTx(ctx context.Context, tx DBTX, ids []string) ([]string, error) {
	ids = canonicalProtectedIDs(ids)
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i := range ids {
		args[i] = ids[i]
	}
	query := `SELECT issue_id FROM protected_issue_records WHERE protection_class <> '' AND issue_id IN (` + placeholders + `) ORDER BY issue_id`
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read protected classifications: %w", err)
	}
	defer rows.Close()
	var found []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan protected classification: %w", err)
		}
		found = append(found, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate protected classifications: %w", err)
	}
	return found, nil
}

// SetProtectedRecordClassInTx creates, changes, or removes the storage-managed
// classification. Callers must first pass RequireProtectedMutation with
// explicitClass set to the requested class (or any non-empty marker for a
// declassification operation).
func SetProtectedRecordClassInTx(ctx context.Context, tx DBTX, id, class string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("%w: protection classification requires an issue ID", storage.ErrValidation)
	}
	if class == "" {
		_, err := tx.ExecContext(ctx, `DELETE FROM protected_issue_records WHERE issue_id = ?`, id)
		return err
	}
	if !validProtectionClass(class) {
		return fmt.Errorf("%w: invalid protection class", storage.ErrValidation)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO protected_issue_records (issue_id, protection_class)
		VALUES (?, ?) ON DUPLICATE KEY UPDATE protection_class = VALUES(protection_class)`, id, class)
	return err
}

// RefuseProtectedDeletionInTx is the default for deletion paths that have no
// request-scoped permit. It must run after closure resolution and before any
// dependent, reference, history, or row mutation.
func RefuseProtectedDeletionInTx(ctx context.Context, tx DBTX, ids []string) error {
	protected, err := ProtectedRecordIDsInTx(ctx, tx, ids)
	if err != nil {
		return err
	}
	if len(protected) > 0 {
		return fmt.Errorf("%w: deletion set contains %s", publicops.ErrProtectedRecord, strings.Join(protected, ", "))
	}
	return nil
}

// DeleteProtectedClassificationsInTx removes classifications only for rows
// that the enclosing transaction has already committed to deleting.
func DeleteProtectedClassificationsInTx(ctx context.Context, tx DBTX, ids []string) error {
	ids = canonicalProtectedIDs(ids)
	for start := 0; start < len(ids); start += deleteBatchSize {
		end := start + deleteBatchSize
		if end > len(ids) {
			end = len(ids)
		}
		inClause, args := buildSQLInClause(ids[start:end])
		if _, err := tx.ExecContext(ctx, `DELETE FROM protected_issue_records WHERE issue_id IN (`+inClause+`)`, args...); err != nil {
			return fmt.Errorf("delete protected classifications: %w", err)
		}
	}
	return nil
}

func canonicalProtectedIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id != "" {
			seen[id] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for id := range seen {
		result = append(result, id)
	}
	sort.Strings(result)
	return result
}

// DependencyEndpointsForIDsInTx returns both endpoints of every edge that
// touches ids, across durable and wisp dependency tables. Structural moves
// and renames use this before their first write so a protected neighbor cannot
// be changed indirectly.
func DependencyEndpointsForIDsInTx(ctx context.Context, tx DBTX, ids []string) ([]string, error) {
	ids = canonicalProtectedIDs(ids)
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids)*2)
	for _, id := range ids {
		args = append(args, id)
	}
	for _, id := range ids {
		args = append(args, id)
	}
	var endpoints []string
	for _, table := range []string{"dependencies", "wisp_dependencies"} {
		query := fmt.Sprintf(`SELECT issue_id, %s FROM %s WHERE issue_id IN (%s) OR %s IN (%s)`, DepTargetExpr, table, placeholders, DepTargetExpr, placeholders)
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			if table == "wisp_dependencies" && isTableNotExistError(err) {
				continue
			}
			return nil, fmt.Errorf("read dependency endpoints touching protected mutation: %w", err)
		}
		for rows.Next() {
			var source, target string
			if err := rows.Scan(&source, &target); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan dependency endpoints: %w", err)
			}
			endpoints = append(endpoints, source, target)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("iterate dependency endpoints: %w", err)
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("close dependency endpoints: %w", err)
		}
	}
	return canonicalProtectedIDs(append(endpoints, ids...)), nil
}

func validProtectionClass(class string) bool {
	return class != "" && strings.TrimSpace(class) == class && len(class) <= 64
}
