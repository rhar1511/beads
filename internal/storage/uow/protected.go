package uow

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/steveyegge/beads/internal/storage/domain"
	"github.com/steveyegge/beads/internal/storage/domain/db"
	storageissueops "github.com/steveyegge/beads/internal/storage/issueops"
	publicops "github.com/steveyegge/beads/issueops"
)

type resolvedDeleteAuthorizationKey struct{}

type resolvedDeleteAuthorization struct {
	permit publicops.ProtectedMutationPermit
	digest string
}

func withResolvedDeleteAuthorization(ctx context.Context, req publicops.DeleteRequest) context.Context {
	unsigned := req
	unsigned.ProtectedPermit = ""
	digest, _ := publicops.ProtectedMutationDigest(unsigned)
	return context.WithValue(ctx, resolvedDeleteAuthorizationKey{}, resolvedDeleteAuthorization{
		permit: req.ProtectedPermit,
		digest: digest,
	})
}

func protectedResolvedDeletionHook(runner db.Runner) domain.ResolvedDeletionHook {
	return func(ctx context.Context, deletion domain.ResolvedDeletion) error {
		resourceIDs := append([]string(nil), deletion.IDs...)
		resourceIDs = append(resourceIDs, deletion.AffectedIDs...)
		resourceIDs = canonicalResolvedDeleteIDs(resourceIDs)

		if deletion.DryRun {
			return storageissueops.RefuseProtectedDeletionInTx(ctx, runner, resourceIDs)
		}
		authorization, _ := ctx.Value(resolvedDeleteAuthorizationKey{}).(resolvedDeleteAuthorization)
		if err := storageissueops.RequireProtectedMutation(ctx, runner, authorization.permit,
			"issue.delete", resourceIDs, authorization.digest, ""); err != nil {
			return err
		}

		// Classification belongs to the rows being deleted. Surviving protected
		// endpoints may have authorized side effects, but they remain classified.
		deletedIDs := canonicalResolvedDeleteIDs(deletion.IDs)
		if err := storageissueops.DeleteProtectedClassificationsInTx(ctx, runner, deletedIDs); err != nil {
			return err
		}
		return nil
	}
}

func canonicalResolvedDeleteIDs(ids []string) []string {
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
	// Stable scope order keeps permit verification and failure messages
	// deterministic across backends.
	sort.Strings(result)
	return result
}

type protectedMutationRunner interface {
	protectedMutationRunner() db.Runner
}

func requireProtectedUOWMutation(ctx context.Context, uw UnitOfWork, permit publicops.ProtectedMutationPermit, operation string, ids []string, digest, explicitClass string) error {
	unwrapped := unwrapUOW(uw)
	provider, ok := unwrapped.(protectedMutationRunner)
	if !ok {
		// Legacy in-memory UOW test doubles have no SQL runner and therefore
		// cannot contain storage-managed protected records. Any request that
		// carries a permit, creates a classification, or has backend authority
		// configured must still fail closed.
		if permit == "" && explicitClass == "" && !storageissueops.ProtectedMutationAuthorityConfigured(ctx) {
			return nil
		}
		return fmt.Errorf("%w: unit of work has no transaction-local protected-record checks", publicops.ErrProtectedRecord)
	}
	return storageissueops.RequireProtectedMutation(ctx, provider.protectedMutationRunner(), permit, operation, ids, digest, explicitClass)
}

func classifyProtectedUOWRecord(ctx context.Context, uw UnitOfWork, id, class string) error {
	unwrapped := unwrapUOW(uw)
	provider, ok := unwrapped.(protectedMutationRunner)
	if !ok {
		return fmt.Errorf("%w: unit of work has no transaction-local classification storage", publicops.ErrProtectedRecord)
	}
	return storageissueops.SetProtectedRecordClassInTx(ctx, provider.protectedMutationRunner(), id, class)
}

func protectedUOWRecords(ctx context.Context, uw UnitOfWork, ids []string) ([]string, error) {
	unwrapped := unwrapUOW(uw)
	provider, ok := unwrapped.(protectedMutationRunner)
	if !ok {
		if !storageissueops.ProtectedMutationAuthorityConfigured(ctx) {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: unit of work has no transaction-local protected-record checks", publicops.ErrProtectedRecord)
	}
	return storageissueops.ProtectedRecordIDsInTx(ctx, provider.protectedMutationRunner(), ids)
}
