package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/issueops"
)

// The CLI spelling and refusal vocabulary preserve the contributor's #4682
// contract. The implementation uses the existing transactional Lifecycle and
// Deleter roles, rather than introducing a second conditional-write engine.
const revisionPreconditionExit = 9
const revisionUnsupportedExit = 13

func ifRevisionPrecondition(cmd *cobra.Command) *int64 {
	if !cmd.Flags().Changed("if-revision") {
		return nil
	}
	value, _ := cmd.Flags().GetInt64("if-revision")
	return &value // explicit zero is a precondition, not an omitted flag
}

func registerRevisionPrecondition(cmd *cobra.Command) {
	cmd.Flags().Int64("if-revision", 0, "Apply this single-issue write only if its opaque revision matches; mismatch exits 9, unsupported topology exits 13")
}

// Args runs before the root opens a store or imports/migrates workspace data.
func validateRevisionArguments(cmd *cobra.Command, args []string, argCount int) error {
	if ifRevisionPrecondition(cmd) == nil {
		return nil
	}
	if len(args) != argCount || args[0] == "" {
		return HandleErrorRespectJSON("--if-revision requires exactly one explicit issue ID")
	}
	if cmd.Name() == "update" {
		claim, _ := cmd.Flags().GetBool("claim")
		if claim {
			return HandleErrorRespectJSON("--if-revision cannot be combined with --claim")
		}
		for _, name := range []string{"add-label", "remove-label", "set-labels", "parent"} {
			if cmd.Flags().Changed(name) {
				return HandleErrorRespectJSON("--if-revision cannot be combined with label or parent edits")
			}
		}
	}
	if cmd.Name() == "delete" {
		force, _ := cmd.Flags().GetBool("force")
		if !force {
			return HandleErrorRespectJSON("--if-revision deletes the issue; pass --force to confirm")
		}
		for _, name := range []string{"cascade", "from-file", "dry-run"} {
			if cmd.Flags().Changed(name) {
				return HandleErrorRespectJSON("--if-revision cannot be combined with --%s", name)
			}
		}
	}
	return nil
}

// Reuse the public store/decorator remote boundary. An uncertain listing is a
// failure, never evidence that the store is local. No unguarded fallback exists.
func revisionWriteReady(ctx context.Context, st storage.DoltStorage) error {
	if st == nil {
		return reportRevisionUnsupported("the resolved store lacks conditional-write capability")
	}
	remote, err := hasConfiguredRemote(ctx, st)
	if err != nil {
		return HandleErrorRespectJSON("checking conditional-write topology: %v", err)
	}
	if remote {
		return reportRevisionUnsupported("--if-revision requires a local store without configured Dolt remotes")
	}
	return nil
}

func reportRevisionUnsupported(message string) error {
	return reportRevisionProblem(map[string]interface{}{
		"error": message, "code": "conditional_write_unsupported", "mutates": false,
	}, revisionUnsupportedExit)
}

func reportRevisionRefusal(err error) error {
	return reportRevisionProblem(map[string]interface{}{
		"error": err.Error(), "code": "conditional_write_refused", "mutates": false,
	}, revisionUnsupportedExit)
}

func mapRevisionWriteError(operation string, err error) error {
	var mismatch *issueops.VersionMismatchError
	if errors.As(err, &mismatch) {
		return reportRevisionProblem(map[string]interface{}{
			"error": fmt.Sprintf("%s: %v", operation, err), "code": "precondition_failed",
			"id": mismatch.IssueID, "expected_revision": mismatch.ExpectedVersion,
			"current_revision": mismatch.CurrentVersion, "mutates": false,
		}, revisionPreconditionExit)
	}
	var unsupported *issueops.ErrUnsupported
	if errors.As(err, &unsupported) || errors.Is(err, errors.ErrUnsupported) {
		return reportRevisionUnsupported(fmt.Sprintf("%s: %v", operation, err))
	}
	if errors.Is(err, issueops.ErrProtectedRecord) || errors.Is(err, issueops.ErrValidation) ||
		errors.Is(err, issueops.ErrCloseBlocked) || errors.Is(err, issueops.ErrCloseOpenChildren) ||
		errors.Is(err, issueops.ErrAlreadyClaimed) || errors.Is(err, issueops.ErrNotClaimable) ||
		errors.Is(err, issueops.ErrNotOwner) ||
		errors.Is(err, issueops.ErrAssigneeMismatch) || errors.Is(err, issueops.ErrStatusMismatch) {
		return reportRevisionRefusal(err)
	}
	return HandleErrorRespectJSON("%s: %v", operation, err)
}

func reportRevisionProblem(body map[string]interface{}, exit int) error {
	if jsonOutput {
		// outputJSON's flat struct projection round-trips through interface{}
		// numbers. Emit this already-shaped map directly so signed64 tokens are
		// never converted to float64, in either envelope format.
		var out interface{} = body
		if jsonEnvelopeEnabled() {
			out = map[string]interface{}{"schema_version": JSONSchemaVersion, "data": body}
		} else {
			body["schema_version"] = JSONSchemaVersion
		}
		if err := outputJSONRaw(out); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(os.Stderr, "Error: %s\n", body["error"])
	}
	return &exitError{Code: exit}
}
