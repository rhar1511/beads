package uow

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/rhar1511/beads/internal/storage/domain"
	"github.com/rhar1511/beads/internal/storage/domain/db"
	storageissueops "github.com/rhar1511/beads/internal/storage/issueops"
	"github.com/rhar1511/beads/internal/workapi"
	publicops "github.com/rhar1511/beads/issueops"
)

type resolvedDeleteAuthorizationKey struct{}

type resolvedDeleteAuthorization struct {
	permit                  publicops.ProtectedMutationPermit
	digest                  string
	protectActiveReferences bool
}

func withResolvedDeleteAuthorization(ctx context.Context, req publicops.DeleteRequest) context.Context {
	unsigned := req
	unsigned.ProtectedPermit = ""
	digest, _ := publicops.ProtectedMutationDigest(unsigned)
	return context.WithValue(ctx, resolvedDeleteAuthorizationKey{}, resolvedDeleteAuthorization{
		permit:                  req.ProtectedPermit,
		digest:                  digest,
		protectActiveReferences: req.ProtectActiveReferences,
	})
}

func protectedResolvedDeletionHook(runner db.Runner) domain.ResolvedDeletionHook {
	return func(ctx context.Context, deletion domain.ResolvedDeletion) error {
		resourceIDs := append([]string(nil), deletion.IDs...)
		resourceIDs = append(resourceIDs, deletion.AffectedIDs...)
		resourceIDs = canonicalResolvedDeleteIDs(resourceIDs)

		authorization, _ := ctx.Value(resolvedDeleteAuthorizationKey{}).(resolvedDeleteAuthorization)
		var referenceOwners map[string]bool
		if authorization.protectActiveReferences {
			var err error
			referenceOwners, err = storageissueops.CheckActiveDeleteReferencesInTx(ctx, runner, deletion.IDs)
			if err != nil {
				return err
			}
		}

		if deletion.DryRun {
			return storageissueops.RefuseProtectedDeletionInTx(ctx, runner, resourceIDs)
		}
		if err := storageissueops.RequireProtectedMutation(ctx, runner, authorization.permit,
			"issue.delete", resourceIDs, authorization.digest, ""); err != nil {
			return err
		}

		for _, id := range workapi.SortedDeleteIDs(referenceOwners) {
			if err := storageissueops.BumpIssueRevisionInTx(ctx, runner, id, referenceOwners[id]); err != nil {
				return err
			}
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

func requireAuthorizedUOWMutation(ctx context.Context, uw UnitOfWork, permit publicops.ProtectedMutationPermit, operation string, ids []string, digest string) error {
	unwrapped := unwrapUOW(uw)
	provider, ok := unwrapped.(protectedMutationRunner)
	if !ok {
		return fmt.Errorf("%w: unit of work has no transaction-local protected-record checks", publicops.ErrProtectedRecord)
	}
	return storageissueops.RequireAuthorizedMutation(ctx, provider.protectedMutationRunner(), permit, operation, ids, digest)
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
