package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/rhar1511/beads/internal/metrics"
	"github.com/rhar1511/beads/internal/ui"
	"github.com/rhar1511/beads/issueops"
)

var assignCmd = &cobra.Command{
	Use:     "assign <id> <name>",
	GroupID: "issues",
	Short:   "Assign an issue to someone",
	Long: `Assign an issue to someone.

Shorthand for 'bd update <id> --assignee <name>'.

Refuses to overwrite another actor's live in_progress claim without --force
(bd-98s5c); issues assigned to a claim.pools alias are exempt, matching
--claim. For a holder-aware transfer prefer
'bd update <id> --if-assignee <holder> -a <new>'.

Examples:
  bd assign bd-123 alice
  bd assign bd-123 ""      # unassign`,
	Args: func(cmd *cobra.Command, args []string) error {
		if err := validateRevisionArguments(cmd, args, 2); err != nil {
			return err
		}
		return cobra.ExactArgs(2)(cmd, args)
	},
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		CheckReadonly("assign")

		evt := metrics.NewCommandEvent("assign")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		force, _ := cmd.Flags().GetBool("force")
		expectedVersion := ifRevisionPrecondition(cmd)

		if usesProxiedServer() {
			if expectedVersion != nil {
				return reportRevisionUnsupported("--if-revision is not supported against a proxied server")
			}
			return runAssignProxiedServer(rootCtx, args, force)
		}

		id := args[0]
		assignee := args[1]

		ctx := rootCtx

		result, err := resolveAndGetIssueForMutation(ctx, store, id)
		if err != nil {
			if result != nil {
				result.Close()
			}
			return HandleErrorRespectJSON("resolving %s: %v", id, err)
		}
		if result == nil || result.Issue == nil {
			if result != nil {
				result.Close()
			}
			return HandleErrorRespectJSON("issue %s not found", id)
		}
		defer result.Close()

		issueStore := result.Store

		if err := validateIssueUpdatable(id, result.Issue); err != nil {
			if expectedVersion != nil {
				return reportRevisionRefusal(err)
			}
			return HandleErrorRespectJSON("%s", err)
		}

		// bd-98s5c: bd assign is shorthand for an unguarded assignee update —
		// same live-claim fence as bd update -a.
		if expectedVersion != nil {
			if err := revisionWriteReady(ctx, issueStore); err != nil {
				return err
			}
			ops, err := writeOps(issueStore)
			if err != nil {
				return mapRevisionWriteError("assigning "+id, err)
			}
			opsCtx, err := issueOpsContext(ctx)
			if err != nil {
				return HandleErrorRespectJSON("%v", err)
			}
			_, err = runCommandUpdateMutation(opsCtx, ops, commandUpdateMutation{
				actor: actor, issueID: result.ResolvedID, force: force,
				patch:           issueops.IssuePatch{Assignee: setField(assignee)},
				expectedVersion: expectedVersion,
			})
			if err != nil {
				return mapRevisionWriteError("assigning "+id, err)
			}
		} else {
			if err := validateIssueReassignable(id, result.Issue, actor, assignee,
				storeClaimPoolAliases(ctx, issueStore), force); err != nil {
				return HandleErrorRespectJSON("%s", err)
			}
			updates := map[string]interface{}{"assignee": assignee}
			if err := issueStore.UpdateIssue(ctx, result.ResolvedID, updates, actor); err != nil {
				return HandleErrorRespectJSON("updating %s: %v", id, err)
			}
		}

		if err := commitPendingIfEmbedded(ctx, issueStore, actor, doltAutoCommitParams{
			Command:  "assign",
			IssueIDs: []string{result.ResolvedID},
		}); err != nil {
			return HandleErrorRespectJSON("failed to commit: %v", err)
		}

		SetLastTouchedID(result.ResolvedID)

		updatedIssue, _ := issueStore.GetIssue(ctx, result.ResolvedID)
		title := ""
		if updatedIssue != nil {
			title = updatedIssue.Title
		}
		if jsonOutput {
			if updatedIssue != nil {
				if err := outputJSON(updatedIssue); err != nil {
					return err
				}
			}
		} else {
			if assignee == "" {
				fmt.Printf("%s Unassigned %s\n", ui.RenderPass("✓"), formatFeedbackID(result.ResolvedID, title))
			} else {
				fmt.Printf("%s Assigned %s to %s\n", ui.RenderPass("✓"), formatFeedbackID(result.ResolvedID, title), assignee)
			}
		}
		return nil
	},
}

func init() {
	registerRevisionPrecondition(assignCmd)
	assignCmd.Flags().Bool("force", false, "Allow overwriting another actor's live in_progress claim (use only for abandoned claims — crashed agent, expired lease; prefer bd reclaim)")
	assignCmd.ValidArgsFunction = issueIDCompletion
	rootCmd.AddCommand(assignCmd)
}
