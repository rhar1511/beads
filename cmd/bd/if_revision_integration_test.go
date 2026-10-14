//go:build integration && !windows

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/configfile"
)

// This is the executable/store seam the source-Cobra declaration test cannot
// cover. It consumes the exact prebuilt candidate, one private Dolt server and
// disposable workspace; it never builds a second binary or touches a rig DB.
func TestRevisionCASCLIConformance(t *testing.T) {
	f := newRevisionCLIFixture(t)
	for _, name := range []string{"update", "close", "assign", "delete"} {
		out, code := f.run(t, name, "--help")
		if code != 0 || !strings.Contains(string(out), "--if-revision int") {
			t.Fatalf("%s executable help lacks revision guard: exit=%d\n%s", name, code, out)
		}
	}

	t.Run("private seeds require explicit transaction commit", func(t *testing.T) {
		id := "rvcli-seed-isolation"
		f.create(t, id)
		before := f.row(t, id)
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		conn, err := f.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		var autoCommit int
		if err := conn.QueryRowContext(ctx, "SELECT @@autocommit").Scan(&autoCommit); err != nil {
			t.Fatal(err)
		}
		if autoCommit != 0 {
			t.Fatalf("fixture requires --no-auto-commit, got @@autocommit=%d", autoCommit)
		}
		defer func() {
			if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
				t.Errorf("rollback private seed visibility probe: %v", err)
			}
		}()
		const probe int64 = 9007199254740993
		if _, err := conn.ExecContext(ctx, "UPDATE issues SET row_lock=? WHERE id=?", probe, id); err != nil {
			t.Fatal(err)
		}
		var localVersion int64
		if err := conn.QueryRowContext(ctx, "SELECT row_lock FROM issues WHERE id=?", id).Scan(&localVersion); err != nil {
			t.Fatal(err)
		}
		if localVersion != probe {
			t.Fatalf("uncommitted seed on owning connection = %d, want %d", localVersion, probe)
		}
		// A separate CLI connection must not mistake a private, uncommitted
		// seed for a committed token. This reproduces the D13r2 fixture seam.
		f.unchanged(t, id, before)
		t.Logf("seed isolation: autocommit=%d, owner sees %d, CLI retains %s", autoCommit, localVersion, before.Revision)
	})

	t.Run("update guards writes and noops", func(t *testing.T) {
		id := "rvcli-update"
		f.create(t, id)
		before := f.row(t, id)
		f.ok(t, "update", id, "--if-revision", before.Revision, "--title", "won", "--json")
		after := f.row(t, id)
		if after.Title != "won" || after.Revision == before.Revision {
			t.Fatalf("matching update did not persist/remint: before=%+v after=%+v", before, after)
		}
		for _, edit := range [][]string{{"--title", "lost"}, {}} {
			args := append([]string{"update", id, "--if-revision", before.Revision, "--json"}, edit...)
			f.mismatch(t, id, before.Revision, after.Revision, args...)
			f.unchanged(t, id, after)
		}
		f.ok(t, "update", id, "--if-revision", after.Revision, "--json")
		f.unchanged(t, id, after)
		// Absent and explicit zero are different even on a no-op.
		f.mismatch(t, id, "0", after.Revision, "update", id, "--if-revision", "0", "--json")
		f.ok(t, "update", id, "--json")
		f.unchanged(t, id, after)
	})

	t.Run("labels and parent edits share the revision fence", func(t *testing.T) {
		id, parent := "rvcli-patch", "rvcli-parent"
		f.create(t, id)
		f.create(t, parent)
		before := f.row(t, id)
		f.ok(t, "update", id, "--if-revision", before.Revision, "--add-label", "guarded", "--parent", parent, "--title", "atomic patch", "--json")
		after := f.row(t, id)
		if after.Title != "atomic patch" || after.Revision == before.Revision {
			t.Fatalf("matching aggregate patch did not persist/remint: %+v", after)
		}
		assertReferences := func(label, parentID string) {
			t.Helper()
			var labels, parents int
			if err := f.db.QueryRow("SELECT COUNT(*) FROM labels WHERE issue_id=? AND label=?", id, label).Scan(&labels); err != nil {
				t.Fatal(err)
			}
			if err := f.db.QueryRow("SELECT COUNT(*) FROM dependencies WHERE issue_id=? AND depends_on_issue_id=? AND type='parent-child'", id, parentID).Scan(&parents); err != nil {
				t.Fatal(err)
			}
			if labels != 1 || parents != 1 {
				t.Fatalf("aggregate references: labels=%d parents=%d, want one each", labels, parents)
			}
		}
		assertReferences("guarded", parent)
		f.mismatch(t, id, before.Revision, after.Revision, "update", id, "--if-revision", before.Revision, "--remove-label", "guarded", "--parent", "", "--title", "lost", "--json")
		f.unchanged(t, id, after)
		assertReferences("guarded", parent)
		for _, edit := range [][]string{
			{"--set-labels", "replacement"},
			{"--remove-label", "replacement"},
			{"--parent", ""},
		} {
			current := f.row(t, id)
			args := append([]string{"update", id, "--if-revision", current.Revision, "--json"}, edit...)
			f.ok(t, args...)
			if got := f.row(t, id); got.Revision == current.Revision {
				t.Fatalf("aggregate edit %v did not remint", edit)
			}
		}
	})

	t.Run("assignment force never bypasses revision", func(t *testing.T) {
		id := "rvcli-assign"
		f.create(t, id)
		initial := f.row(t, id)
		f.ok(t, "assign", id, "foreign-holder", "--if-revision", initial.Revision, "--json")
		assigned := f.row(t, id)
		f.ok(t, "update", id, "--status", "in_progress", "--if-revision", assigned.Revision, "--json")
		live := f.row(t, id)
		f.problem(t, 13, "conditional_write_refused", "assign", id, "revision-fixture", "--if-revision", live.Revision, "--json")
		f.unchanged(t, id, live)
		f.mismatch(t, id, assigned.Revision, live.Revision, "assign", id, "revision-fixture", "--if-revision", assigned.Revision, "--force", "--json")
		f.unchanged(t, id, live)
		f.ok(t, "assign", id, "revision-fixture", "--if-revision", live.Revision, "--force", "--json")
		if got := f.row(t, id); got.Assignee != "revision-fixture" || got.Revision == live.Revision {
			t.Fatalf("matching forced assignment = %+v", got)
		}
	})

	t.Run("close checks stale token before forced noop", func(t *testing.T) {
		id := "rvcli-close"
		f.create(t, id)
		before := f.row(t, id)
		out := f.ok(t, "close", id, "--if-revision", before.Revision, "--reason", "first close", "--json")
		closed := f.row(t, id)
		assertRevisionCloseResponse(t, out, closed)
		if closed.Status != "closed" || closed.Revision == before.Revision {
			t.Fatalf("matching close = %+v", closed)
		}
		f.mismatch(t, id, before.Revision, closed.Revision, "close", id, "--if-revision", before.Revision, "--force", "--reason", "overwrite", "--json")
		f.unchanged(t, id, closed)
		out = f.ok(t, "close", id, "--if-revision", closed.Revision, "--reason", "overwrite", "--json")
		assertRevisionCloseResponse(t, out, closed)
		f.unchanged(t, id, closed)
	})

	t.Run("delete refuses stale and dryrun without deleting", func(t *testing.T) {
		id := "rvcli-delete"
		f.create(t, id)
		old := f.row(t, id)
		f.ok(t, "update", id, "--title", "new", "--json")
		current := f.row(t, id)
		f.mismatch(t, id, old.Revision, current.Revision, "delete", id, "--if-revision", old.Revision, "--force", "--json")
		f.unchanged(t, id, current)
		_, code := f.run(t, "delete", id, "--if-revision", current.Revision, "--force", "--dry-run", "--json")
		if code != 1 {
			t.Fatalf("guarded dry-run exit=%d, want argument refusal1", code)
		}
		f.unchanged(t, id, current)
		f.ok(t, "delete", id, "--if-revision", current.Revision, "--force", "--json")
		if out, code := f.run(t, "show", id, "--json"); code == 0 {
			t.Fatalf("matching delete left the row visible: %s", out)
		}
	})

	t.Run("guarded terminal delete preserves active owner and fences cleanup", func(t *testing.T) {
		target, owner := "rvcli-safe-target", "rvcli-safe-owner"
		f.create(t, target)
		f.create(t, owner)
		f.ok(t, "close", target, "--json")
		f.ok(t, "dep", "add", owner, target, "--type", "relates-to", "--json")
		targetBefore, ownerBefore := f.row(t, target), f.row(t, owner)
		edgeCount := func() int {
			t.Helper()
			// The private server has autocommit disabled. Give each observation
			// its own transaction so an earlier read cannot retain a stale snapshot.
			tx, err := f.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
					t.Errorf("end reference observation: %v", err)
				}
			}()
			var edges int
			if err := tx.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM dependencies WHERE issue_id=? AND depends_on_issue_id=?", owner, target).Scan(&edges); err != nil {
				t.Fatal(err)
			}
			return edges
		}
		if edges := edgeCount(); edges != 1 {
			t.Fatalf("fixture did not establish active reference: count=%d", edges)
		}
		args := []string{"delete", target, "--force", "--protect-active-references", "--if-revision", targetBefore.Revision, "--json"}
		f.problem(t, 13, "conditional_write_refused", args...)
		f.unchanged(t, target, targetBefore)
		f.unchanged(t, owner, ownerBefore)
		if edges := edgeCount(); edges != 1 {
			t.Fatalf("refusal lost active reference: count=%d", edges)
		}
		f.ok(t, "close", owner, "--json")
		closedOwner := f.row(t, owner)
		f.ok(t, args...)
		after := f.row(t, owner)
		if after.Revision == closedOwner.Revision {
			t.Fatalf("cleanup reused surviving owner revision: %+v", after)
		}
		if edges := edgeCount(); edges != 0 {
			t.Fatalf("cleanup retained reference: count=%d", edges)
		}
		if out, code := f.run(t, "show", target, "--json"); code == 0 {
			t.Fatalf("guarded deletion retained target: %s", out)
		}
	})

	t.Run("close only earns a next claim after a matching changed write", func(t *testing.T) {
		id, nextID := "rvcli-close-claim", "rvcli-next"
		f.create(t, id)
		f.create(t, nextID)
		f.ok(t, "update", nextID, "--priority", "0", "--json")
		next := f.row(t, nextID)
		before := f.row(t, id)
		f.ok(t, "close", id, "--if-revision", before.Revision, "--json")
		closed := f.row(t, id)
		f.mismatch(t, id, before.Revision, closed.Revision, "close", id, "--if-revision", before.Revision, "--claim-next", "--json")
		f.unchanged(t, nextID, next)
		out := f.ok(t, "close", id, "--if-revision", closed.Revision, "--claim-next", "--json")
		assertRevisionCloseResponse(t, out, closed)
		f.unchanged(t, id, closed)
		f.unchanged(t, nextID, next)
		f.ok(t, "reopen", id, "--json")
		reopened := f.row(t, id)
		out = f.ok(t, "close", id, "--if-revision", reopened.Revision, "--claim-next", "--json")
		var response struct {
			Closed  []revisionCLIRecord `json:"closed"`
			Claimed *revisionCLIRecord  `json:"claimed"`
		}
		if err := json.Unmarshal(revisionCLIData(t, out), &response); err != nil {
			t.Fatalf("close-and-claim response: %v\n%s", err, out)
		}
		assertRevisionClosedRows(t, response.Closed, f.row(t, id))
		if response.Claimed == nil || response.Claimed.ID != nextID || response.Claimed.Status != "in_progress" || response.Claimed.Assignee != "revision-fixture" {
			t.Fatalf("close-and-claim omitted claimed snapshot: %s", out)
		}
		if claimed := f.row(t, nextID); claimed.Status != "in_progress" || claimed.Assignee != "revision-fixture" {
			t.Fatalf("matching changed close did not claim highest-priority next: %+v", claimed)
		}
	})

	t.Run("competing guarded writers have exactly one winner", func(t *testing.T) {
		id := "rvcli-race"
		f.create(t, id)
		before := f.row(t, id)
		type result struct {
			out  []byte
			code int
			err  error
		}
		results := make(chan result, 2)
		for _, title := range []string{"left", "right"} {
			go func(title string) {
				out, code, err := f.execute("update", id, "--title", title, "--if-revision", before.Revision, "--json")
				results <- result{out, code, err}
			}(title)
		}
		wins, losses := 0, 0
		var loser []byte
		// Both commands must be waited even when the first result is a failure.
		// A fatal assertion must never abandon the other child process.
		completed := make([]result, 0, 2)
		for range 2 {
			completed = append(completed, <-results)
		}
		for _, r := range completed {
			if r.err != nil {
				t.Fatalf("competing CLI execution: %v", r.err)
			}
			switch r.code {
			case 0:
				wins++
			case 9:
				losses++
				loser = r.out
			default:
				t.Fatalf("competing CLI exit=%d\n%s", r.code, r.out)
			}
		}
		if wins != 1 || losses != 1 {
			t.Fatalf("wins=%d losses=%d, want one each", wins, losses)
		}
		after := f.row(t, id)
		if after.Title != "left" && after.Title != "right" {
			t.Fatalf("winner row = %+v", after)
		}
		assertRevisionMismatchBody(t, loser, id, before.Revision, after.Revision)
	})

	t.Run("authored aggregate edits invalidate earlier token", func(t *testing.T) {
		id, other := "rvcli-aggregate", "rvcli-dependency"
		f.create(t, id)
		f.create(t, other)
		for _, edit := range [][]string{
			{"update", id, "--add-label", "authored"},
			{"dep", "add", id, other},
			{"comments", "add", id, "authored comment"},
		} {
			before := f.row(t, id)
			f.ok(t, edit...)
			after := f.row(t, id)
			if after.Revision == before.Revision {
				t.Fatalf("aggregate edit %v did not invalidate token", edit)
			}
			f.mismatch(t, id, before.Revision, after.Revision, "update", id, "--if-revision", before.Revision, "--title", "lost", "--json")
			f.unchanged(t, id, after)
		}
	})

	t.Run("signed64 wire values remain lossless in both envelopes", func(t *testing.T) {
		id := "rvcli-large"
		f.create(t, id)
		// Seed an otherwise valid private fixture token at the precision edge.
		// Assertions and subsequent mutations all use the public executable.
		const large int64 = 9007199254740993
		f.seed(t, "UPDATE issues SET row_lock=? WHERE id=?", large, id)
		before := f.row(t, id)
		if before.Revision != strconv.FormatInt(large, 10) {
			t.Fatalf("detail revision = %q", before.Revision)
		}
		for _, envelope := range []string{"0", "1"} {
			wrapped := *f
			wrapped.env = append(append([]string{}, f.env...), "BD_JSON_ENVELOPE="+envelope)
			for _, stale := range []string{"0", "-9223372036854775808", "9223372036854775807"} {
				wrapped.mismatch(t, id, stale, before.Revision, "update", id, "--if-revision", stale, "--force", "--title", "lost", "--json")
				f.unchanged(t, id, before)
			}
		}
		f.ok(t, "update", id, "--if-revision", before.Revision, "--title", "large matched", "--json")
		if after := f.row(t, id); after.Title != "large matched" || after.Revision == before.Revision {
			t.Fatalf("large matching write = %+v", after)
		}
	})

	t.Run("protected classification remains independent of force and revision", func(t *testing.T) {
		id := "rvcli-protected"
		f.create(t, id)
		// Classification is controlled outside ordinary issue edits. Seed only
		// that private fixture input, then exercise refusal through the CLI.
		f.seed(t, "INSERT INTO protected_issue_records (issue_id, protection_class) VALUES (?,?)", id, "source")
		before := f.row(t, id)
		for _, args := range [][]string{
			{"update", id, "--title", "lost", "--if-revision", before.Revision, "--force", "--json"},
			{"assign", id, "other", "--if-revision", before.Revision, "--force", "--json"},
			{"close", id, "--if-revision", before.Revision, "--force", "--json"},
			{"delete", id, "--if-revision", before.Revision, "--force", "--json"},
		} {
			f.problem(t, 13, "conditional_write_refused", args...)
			f.unchanged(t, id, before)
		}
	})

	t.Run("proxy refuses before provider startup", func(t *testing.T) {
		id := "rvcli-proxy"
		f.create(t, id)
		before := f.row(t, id)
		beadsDir := filepath.Join(f.workspace, ".beads")
		path := filepath.Join(beadsDir, configfile.ConfigFileName)
		original, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Errorf("restore private fixture metadata: %v", err)
			}
		})
		cfg, err := configfile.Load(beadsDir)
		if err != nil || cfg == nil {
			t.Fatalf("load private fixture metadata: %v", err)
		}
		cfg.DoltMode = configfile.DoltModeProxiedServer
		if err := cfg.Save(beadsDir); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{
			{"update", id, "--title", "lost", "--if-revision", before.Revision, "--json"},
			{"assign", id, "other", "--if-revision", before.Revision, "--json"},
			{"close", id, "--if-revision", before.Revision, "--json"},
			{"delete", id, "--if-revision", before.Revision, "--force", "--json"},
		} {
			f.problem(t, 13, "conditional_write_unsupported", args...)
		}
		if _, err := os.Stat(configfile.ProxiedServerClientInfoPath(beadsDir)); !os.IsNotExist(err) {
			t.Fatalf("refused proxy opened a provider: client info stat=%v", err)
		}
		if err := os.WriteFile(path, original, 0o600); err != nil {
			t.Fatal(err)
		}
		f.unchanged(t, id, before)
	})

	t.Run("configured remote refuses without unguarded fallback", func(t *testing.T) {
		id := "rvcli-remote"
		f.create(t, id)
		before := f.row(t, id)
		f.ok(t, "dolt", "remote", "add", "fixture", "file://"+filepath.Join(t.TempDir(), "remote"))
		for _, args := range [][]string{
			{"update", id, "--title", "lost", "--if-revision", before.Revision, "--json"},
			{"assign", id, "other", "--if-revision", before.Revision, "--json"},
			{"close", id, "--if-revision", before.Revision, "--json"},
			{"delete", id, "--if-revision", before.Revision, "--force", "--json"},
		} {
			f.problem(t, 13, "conditional_write_unsupported", args...)
			f.unchanged(t, id, before)
		}
	})
}

type revisionCLIRecord struct {
	ID, Title, Status, Assignee, Revision string
	CloseReason                           string `json:"close_reason"`
}

type revisionCLIFixture struct {
	bd, workspace string
	env           []string
	db            *sql.DB
}

func newRevisionCLIFixture(t *testing.T) *revisionCLIFixture {
	t.Helper()
	bd := os.Getenv("BEADS_TEST_BD_BINARY")
	if bd == "" || os.Getenv("BEADS_TEST_ENV_RUN_DOLT") != "1" {
		t.Fatal("explicit real Dolt and exact BEADS_TEST_BD_BINARY are required; no skip/build fallback")
	}
	data, err := os.ReadFile(bd)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("candidate bd SHA256=%x", sha256.Sum256(data))
	dolt, err := q43DoltBinary()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	home, doltRoot, doltData, workspace := filepath.Join(root, "home"), filepath.Join(root, "dolt-root"), filepath.Join(root, "data"), filepath.Join(root, "workspace")
	for _, dir := range []string{home, doltRoot, doltData, workspace} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	env := q43ProcessEnv(home, doltRoot)
	q43Run(t, 30*time.Second, env, doltData, dolt, "config", "--global", "--add", "user.name", "Revision fixture")
	q43Run(t, 30*time.Second, env, doltData, dolt, "config", "--global", "--add", "user.email", "revision@example.invalid")
	q43Run(t, 90*time.Second, env, doltData, dolt, "init")
	port := q43FreePort(t)
	process := q43StartDoltServer(t, env, dolt, doltData, port)
	q43WaitForDolt(t, port, process)
	q43Run(t, 30*time.Second, env, workspace, "git", "init", "--quiet")
	q43Run(t, 30*time.Second, env, workspace, "git", "config", "user.name", "Revision fixture")
	q43Run(t, 30*time.Second, env, workspace, "git", "config", "user.email", "revision@example.invalid")
	q43Run(t, 30*time.Second, env, workspace, "git", "config", "core.hooksPath", ".git/hooks")
	database := q43UniqueDatabase(t)
	q43Run(t, 4*time.Minute, env, workspace, bd, "init", "--quiet", "--server", "--server-host", "127.0.0.1", "--server-port", strconv.Itoa(port), "--database", database, "--prefix", "rvcli", "--non-interactive", "--skip-agents", "--skip-hooks")
	db, err := sql.Open("mysql", fmt.Sprintf("root@tcp(127.0.0.1:%d)/%s", port, database))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close private fixture SQL handle: %v", err)
		}
	})
	return &revisionCLIFixture{bd: bd, workspace: workspace, env: env, db: db}
}

// seed commits fixture-only SQL inputs before a separate CLI connection reads
// them. The owned Dolt server disables session autocommit; DB.Exec alone leaves
// these inputs private to its connection and tests an ordinary record instead.
func (f *revisionCLIFixture) seed(t *testing.T, query string, args ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	tx, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			t.Errorf("rollback private fixture seed: %v", err)
		}
	}()
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		t.Fatalf("private fixture seed affected %d rows, want 1: %v", n, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit private fixture seed: %v", err)
	}
}

func (f *revisionCLIFixture) execute(args ...string) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, f.bd, append([]string{"--actor", "revision-fixture"}, args...)...)
	cmd.Dir, cmd.Env = f.workspace, f.env
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return out, -1, ctx.Err()
	}
	if err == nil {
		return out, 0, nil
	}
	var exited *exec.ExitError
	if errors.As(err, &exited) && exited.ExitCode() >= 0 {
		return out, exited.ExitCode(), nil
	}
	return out, -1, err
}

func (f *revisionCLIFixture) run(t *testing.T, args ...string) ([]byte, int) {
	t.Helper()
	out, code, err := f.execute(args...)
	if err != nil {
		t.Fatalf("CLI %v did not exit normally: %v\n%s", args, err, out)
	}
	return out, code
}

func (f *revisionCLIFixture) ok(t *testing.T, args ...string) []byte {
	t.Helper()
	out, code := f.run(t, args...)
	if code != 0 {
		t.Fatalf("CLI %v exit=%d\n%s", args, code, out)
	}
	return out
}

func (f *revisionCLIFixture) create(t *testing.T, id string) {
	t.Helper()
	f.ok(t, "create", "fixture", "--id", id, "--description", "private revision fixture", "--json")
}

func revisionCLIData(t *testing.T, raw []byte) []byte {
	t.Helper()
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '{' {
		var envelope struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatalf("invalid JSON response: %v\n%s", err, raw)
		}
		if envelope.Data != nil {
			return envelope.Data
		}
	}
	return raw
}

func (f *revisionCLIFixture) row(t *testing.T, id string) revisionCLIRecord {
	t.Helper()
	raw := revisionCLIData(t, f.ok(t, "show", id, "--json"))
	var rows []revisionCLIRecord
	if err := json.Unmarshal(raw, &rows); err != nil || len(rows) != 1 || rows[0].Revision == "" {
		t.Fatalf("detail row/revision unavailable: %v\n%s", err, raw)
	}
	return rows[0]
}

func (f *revisionCLIFixture) unchanged(t *testing.T, id string, want revisionCLIRecord) {
	t.Helper()
	if got := f.row(t, id); got != want {
		t.Fatalf("refusal/noop changed row: got=%+v want=%+v", got, want)
	}
}

func assertRevisionCloseResponse(t *testing.T, raw []byte, want revisionCLIRecord) {
	t.Helper()
	var rows []revisionCLIRecord
	if err := json.Unmarshal(revisionCLIData(t, raw), &rows); err != nil {
		t.Fatalf("close response must retain its ordinary issue array: %v\n%s", err, raw)
	}
	assertRevisionClosedRows(t, rows, want)
}

func assertRevisionClosedRows(t *testing.T, rows []revisionCLIRecord, want revisionCLIRecord) {
	t.Helper()
	if len(rows) != 1 {
		t.Fatalf("close omitted its post-state snapshot: %+v", rows)
	}
	// Revision is published by detail reads, not generic issue JSON. Keep the
	// existing close wire shape while comparing its persisted visible fields.
	want.Revision = ""
	if rows[0] != want || rows[0].Status != "closed" {
		t.Fatalf("close response does not describe persisted post-state: got=%+v want=%+v", rows[0], want)
	}
}

func (f *revisionCLIFixture) problem(t *testing.T, wantExit int, wantCode string, args ...string) []byte {
	t.Helper()
	out, code := f.run(t, args...)
	if code != wantExit {
		t.Fatalf("CLI %v exit=%d want=%d\n%s", args, code, wantExit, out)
	}
	var body struct {
		Code     string `json:"code"`
		Mutates  *bool  `json:"mutates"`
		Expected *int64 `json:"expected_revision"`
		Current  *int64 `json:"current_revision"`
	}
	if err := json.Unmarshal(revisionCLIData(t, out), &body); err != nil || body.Code != wantCode {
		t.Fatalf("coded refusal=%q want=%q err=%v\n%s", body.Code, wantCode, err, out)
	}
	if body.Mutates == nil || *body.Mutates {
		t.Fatalf("refusal must explicitly report mutates:false: %s", out)
	}
	if wantCode != "precondition_failed" && (body.Expected != nil || body.Current != nil) {
		t.Fatalf("non-mismatch refusal must not invent revision tokens: %s", out)
	}
	return out
}

func (f *revisionCLIFixture) mismatch(t *testing.T, id, expected, current string, args ...string) {
	t.Helper()
	out := f.problem(t, 9, "precondition_failed", args...)
	assertRevisionMismatchBody(t, out, id, expected, current)
}

func assertRevisionMismatchBody(t *testing.T, raw []byte, id, expected, current string) {
	t.Helper()
	var body struct {
		ID       string `json:"id"`
		Expected *int64 `json:"expected_revision"`
		Current  *int64 `json:"current_revision"`
	}
	if err := json.Unmarshal(revisionCLIData(t, raw), &body); err != nil {
		t.Fatalf("integer revision envelope: %v\n%s", err, raw)
	}
	if body.ID != id || body.Expected == nil || body.Current == nil || strconv.FormatInt(*body.Expected, 10) != expected || strconv.FormatInt(*body.Current, 10) != current {
		t.Fatalf("transaction-owned mismatch envelope does not preserve tokens: %s", raw)
	}
}
