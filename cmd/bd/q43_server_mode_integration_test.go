//go:build integration && !windows

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/rhar1511/beads/httpclient"
	"github.com/rhar1511/beads/internal/configfile"
	"github.com/rhar1511/beads/internal/storage/dbproxy/proxy"
)

const (
	q43IntegrationToken = "q43-disposable-integration-token"
	q43IssueMetadataKey = "q43_transition_state"
)

// TestQ43ServerModeTransitionConformance exercises the transition contract
// through a CGO-free bd serve process connected to a private local Dolt SQL
// server. It covers the real startup readiness gate, API client handshake,
// durable receipt read, exact replay, bd serve restart, concurrent duplicate
// requests, and recovery after the client loses a successful POST response.
//
// This test is intentionally opt-in (`-tags=integration`) because it builds a
// second bd binary and starts a local Dolt server. Every file, process, port,
// workspace, and database it creates is private to t.TempDir().
func TestQ43ServerModeTransitionConformance(t *testing.T) {
	doltBinary, err := q43DoltBinary()
	if err != nil {
		t.Skipf("Q43 server-mode conformance needs the Dolt CLI: %v", err)
	}

	root := t.TempDir()
	home := filepath.Join(root, "home")
	doltRoot := filepath.Join(root, "dolt-root")
	doltData := filepath.Join(root, "dolt-data")
	for _, dir := range []string{home, doltRoot, doltData} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("create disposable directory %s: %v", dir, err)
		}
	}
	env := q43ProcessEnv(home, doltRoot)
	q43Run(t, 30*time.Second, env, doltData, doltBinary, "config", "--global", "--add", "user.name", "Q43 integration")
	q43Run(t, 30*time.Second, env, doltData, doltBinary, "config", "--global", "--add", "user.email", "q43-integration@example.invalid")
	q43Run(t, 90*time.Second, env, doltData, doltBinary, "init")

	doltPort := q43FreePort(t)
	dolt := q43StartDoltServer(t, env, doltBinary, doltData, doltPort)
	q43WaitForDolt(t, doltPort, dolt)

	bd := q43BuildBD(t, root, home, doltRoot)
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("create disposable workspace: %v", err)
	}
	q43Run(t, 30*time.Second, env, workspace, "git", "init", "--quiet")
	q43Run(t, 30*time.Second, env, workspace, "git", "config", "user.name", "Q43 integration")
	q43Run(t, 30*time.Second, env, workspace, "git", "config", "user.email", "q43-integration@example.invalid")
	q43Run(t, 30*time.Second, env, workspace, "git", "config", "core.hooksPath", ".git/hooks")

	database := q43UniqueDatabase(t)
	q43Run(t, 4*time.Minute, env, workspace, bd,
		"init", "--quiet", "--server",
		"--server-host", "127.0.0.1", "--server-port", strconv.Itoa(doltPort),
		"--database", database, "--prefix", "q43",
		"--non-interactive", "--skip-agents", "--skip-hooks")
	beadsDir := filepath.Join(workspace, ".beads")
	project, err := configfile.Load(beadsDir)
	if err != nil {
		t.Fatalf("load disposable workspace identity: %v", err)
	}
	if project == nil || strings.TrimSpace(project.ProjectID) == "" {
		t.Fatal("bd init did not persist a project ID for the disposable workspace")
	}
	if got := project.DoltDatabase; got != database {
		t.Fatalf("configured Dolt database = %q, want %q", got, database)
	}
	tokenFile := filepath.Join(root, "serve-token")
	if err := os.WriteFile(tokenFile, []byte(q43IntegrationToken+"\n"), 0o600); err != nil {
		t.Fatalf("write disposable serve token: %v", err)
	}
	proxyRoot := filepath.Join(beadsDir, "dolt")
	t.Cleanup(func() {
		if err := proxy.Shutdown(proxyRoot); err != nil {
			t.Errorf("stop disposable bd serve proxy: %v", err)
		}
	})
	// The SQL proxy survives a bd serve restart. Own it here, rather than
	// letting a short-lived serve process detach it and lose its Wait owner.
	// Register after the shutdown postcheck so cleanup stops and waits this
	// child before that postcheck can inspect its record.
	privateProxy := q43StartPrivateProxy(t, bd, workspace, env, proxyRoot, doltPort)

	issueID := strings.TrimSpace(q43Run(t, 2*time.Minute, env, workspace, bd,
		"create", "--silent", "Q43 transition conformance", "-p", "2"))
	if issueID == "" {
		t.Fatal("bd create returned an empty issue ID")
	}

	serve := q43StartServe(t, bd, workspace, env, tokenFile)
	baseURL := serve.baseURL()
	client := q43Client(t, baseURL, project.ProjectID, database, http.DefaultClient)
	q43AssertReadyContext(t, client, baseURL, project.ProjectID, database)

	issueRevision := q43IssueRevision(t, baseURL, project.ProjectID, issueID)
	request := q43TransitionRequest("q43-receipt-initial", "initial", issueRevision)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	first, err := client.TransitionMetadata(ctx, issueID, request)
	cancel()
	if err != nil {
		t.Fatalf("initial Q43 transition: %v", err)
	}
	if !first.Applied || first.Replayed || first.Receipt == nil {
		t.Fatalf("initial transition = %+v, want applied, non-replayed receipt", first)
	}
	q43AssertReceipt(t, first.Receipt, issueID, request)
	updatedRevision := q43AssertIssueEffect(t, baseURL, project.ProjectID, issueID, request)
	if updatedRevision != first.Receipt.ToVersion {
		t.Fatalf("issue revision after transition = %d, want receipt to_version %d", updatedRevision, first.Receipt.ToVersion)
	}

	stored, err := client.GetTransitionReceipt(t.Context(), request.ReceiptID)
	if err != nil {
		t.Fatalf("read Q43 receipt: %v", err)
	}
	q43AssertReceipt(t, stored, issueID, request)

	ctx, cancel = context.WithTimeout(t.Context(), 90*time.Second)
	replay, err := client.TransitionMetadata(ctx, issueID, request)
	cancel()
	if err != nil {
		t.Fatalf("exact Q43 replay: %v", err)
	}
	if !replay.Applied || !replay.Replayed || replay.Receipt == nil || replay.Receipt.ToVersion != stored.ToVersion {
		t.Fatalf("exact replay = %+v, want replay of receipt ending at %d", replay, stored.ToVersion)
	}
	if got := q43AssertIssueEffect(t, baseURL, project.ProjectID, issueID, request); got != updatedRevision {
		t.Fatalf("revision after exact replay = %d, want unchanged %d", got, updatedRevision)
	}
	q43AssertSourceSnapshot(t, client, baseURL, project.ProjectID, issueID, updatedRevision, nil)

	firstTargetID := strings.TrimSpace(q43Run(t, 2*time.Minute, env, workspace, bd,
		"create", "--silent", "Q43 source snapshot first target", "-p", "2"))
	secondTargetID := strings.TrimSpace(q43Run(t, 2*time.Minute, env, workspace, bd,
		"create", "--silent", "Q43 source snapshot second target", "-p", "2"))
	if firstTargetID == "" || secondTargetID == "" {
		t.Fatalf("bd create returned empty source-snapshot target IDs: %q, %q", firstTargetID, secondTargetID)
	}
	q43Run(t, 2*time.Minute, env, workspace, bd, "dep", "add", issueID, firstTargetID)
	q43Run(t, 2*time.Minute, env, workspace, bd, "dep", "add", issueID, secondTargetID, "--type", "related")
	edgeRevision := q43IssueRevision(t, baseURL, project.ProjectID, issueID)
	edges := map[string]string{firstTargetID: "blocks", secondTargetID: "related"}
	q43AssertSourceSnapshot(t, client, baseURL, project.ProjectID, issueID, edgeRevision, edges)

	// Two simultaneous copies of one request must resolve to one applied write
	// and one replay, with a single resulting revision.
	concurrentID := strings.TrimSpace(q43Run(t, 2*time.Minute, env, workspace, bd,
		"create", "--silent", "Q43 concurrent transition", "-p", "2"))
	if concurrentID == "" {
		t.Fatal("bd create returned an empty concurrent issue ID")
	}
	concurrentRevision := q43IssueRevision(t, baseURL, project.ProjectID, concurrentID)
	concurrentRequest := q43TransitionRequest("q43-receipt-concurrent", "concurrent", concurrentRevision)
	concurrentProxy, concurrentArrivals := q43ConcurrentTransitionProxy(t, baseURL, concurrentRequest.ReceiptID)
	concurrentClient := q43Client(t, concurrentProxy.URL, project.ProjectID, database, concurrentProxy.Client())
	concurrentReceiptRevision := q43AssertConcurrentReplay(t, concurrentClient, concurrentID, concurrentRequest)
	if got := concurrentArrivals.Load(); got != 2 {
		t.Fatalf("concurrent ingress observed %d matching POSTs, want 2", got)
	}
	concurrentAfter := q43AssertIssueEffect(t, baseURL, project.ProjectID, concurrentID, concurrentRequest)
	if concurrentAfter != concurrentReceiptRevision {
		t.Fatalf("issue revision after concurrent transition = %d, want receipt to_version %d", concurrentAfter, concurrentReceiptRevision)
	}

	if err := serve.stop(); err != nil {
		t.Fatalf("cleanly stop first bd serve process: %v", err)
	}
	serve = q43StartServe(t, bd, workspace, env, tokenFile)
	baseURL = serve.baseURL()
	client = q43Client(t, baseURL, project.ProjectID, database, http.DefaultClient)
	q43AssertReadyContext(t, client, baseURL, project.ProjectID, database)
	t.Run("private_proxy_remains_owned_across_serve_restart", func(t *testing.T) {
		q43AssertOwnedProxyRunning(t, proxyRoot, privateProxy)
	})
	restartedReceipt, err := client.GetTransitionReceipt(t.Context(), request.ReceiptID)
	if err != nil {
		t.Fatalf("read Q43 receipt after clean bd serve restart: %v", err)
	}
	q43AssertReceipt(t, restartedReceipt, issueID, request)
	if restartedReceipt.ToVersion != stored.ToVersion {
		t.Fatalf("receipt to_version after restart = %d, want %d", restartedReceipt.ToVersion, stored.ToVersion)
	}
	if got := q43AssertIssueEffect(t, baseURL, project.ProjectID, issueID, request); got != edgeRevision {
		t.Fatalf("issue revision after bd serve restart = %d, want %d", got, edgeRevision)
	}
	q43AssertSourceSnapshot(t, client, baseURL, project.ProjectID, issueID, edgeRevision, edges)

	// The proxy forwards the POST to the real server, reads its successful
	// response, then closes the client connection before returning any bytes.
	// The client therefore loses a successful result, recovers by receipt ID,
	// and sends the exact request again to verify the persisted replay path.
	lostResponseID := strings.TrimSpace(q43Run(t, 2*time.Minute, env, workspace, bd,
		"create", "--silent", "Q43 lost response transition", "-p", "2"))
	if lostResponseID == "" {
		t.Fatal("bd create returned an empty lost-response issue ID")
	}
	lostResponseRevision := q43IssueRevision(t, baseURL, project.ProjectID, lostResponseID)
	lostResponseRequest := q43TransitionRequest("q43-receipt-lost-response", "lost-response", lostResponseRevision)
	faultProxy, dropped, dropErr := q43LostResponseProxy(t, baseURL)
	faultClient := q43Client(t, faultProxy.URL, project.ProjectID, database, faultProxy.Client())
	ctx, cancel = context.WithTimeout(t.Context(), 90*time.Second)
	_, lostErr := faultClient.TransitionMetadata(ctx, lostResponseID, lostResponseRequest)
	cancel()
	if lostErr == nil {
		t.Fatal("transition unexpectedly returned a response through the fault proxy")
	}
	if !dropped.Load() {
		select {
		case err := <-dropErr:
			t.Fatalf("fault proxy did not discard the successful response: %v (client error: %v)", err, lostErr)
		default:
			t.Fatalf("fault proxy did not observe a successful transition response (client error: %v)", lostErr)
		}
	}
	ctx, cancel = context.WithTimeout(t.Context(), 90*time.Second)
	recovered, found, err := faultClient.RecoverTransition(ctx, lostResponseID, lostResponseRequest)
	cancel()
	if err != nil || !found || recovered == nil {
		t.Fatalf("recover lost Q43 response = (%+v, %v, %v), want exact durable receipt", recovered, found, err)
	}
	q43AssertReceipt(t, recovered, lostResponseID, lostResponseRequest)
	ctx, cancel = context.WithTimeout(t.Context(), 90*time.Second)
	lostReplay, err := faultClient.TransitionMetadata(ctx, lostResponseID, lostResponseRequest)
	cancel()
	if err != nil {
		t.Fatalf("retry exact request after receipt recovery: %v", err)
	}
	if !lostReplay.Applied || !lostReplay.Replayed || lostReplay.Receipt == nil || lostReplay.Receipt.ToVersion != recovered.ToVersion {
		t.Fatalf("lost-response retry = %+v, want replay of recovered receipt", lostReplay)
	}
	if got := q43AssertIssueEffect(t, baseURL, project.ProjectID, lostResponseID, lostResponseRequest); got != recovered.ToVersion {
		t.Fatalf("issue revision after lost-response recovery = %d, want receipt to_version %d", got, recovered.ToVersion)
	}
	t.Run("private_proxy_is_waited_before_cleanup", func(t *testing.T) {
		if err := serve.stop(); err != nil {
			t.Fatalf("stop final bd serve before private proxy: %v", err)
		}
		q43AssertOwnedProxyRunning(t, proxyRoot, privateProxy)
		if err := privateProxy.stop(60 * time.Second); err != nil {
			t.Fatalf("stop and wait private proxy: %v", err)
		}
		if state := privateProxy.cmd.ProcessState; state == nil || !state.Exited() || state.ExitCode() != 0 {
			t.Fatalf("private proxy Wait state = %v, want normal exit 0", state)
		}
		if running, pid := proxy.IsRunning(proxyRoot); running || pid != 0 {
			t.Fatalf("private proxy discovery after Wait = (%v, %d), want (false, 0)", running, pid)
		}
	})
}

func q43DoltBinary() (string, error) {
	if configured := strings.TrimSpace(os.Getenv("BEADS_TEST_DOLT_BINARY")); configured != "" {
		path, err := exec.LookPath(configured)
		if err != nil {
			return "", fmt.Errorf("BEADS_TEST_DOLT_BINARY: %w", err)
		}
		return path, nil
	}
	return exec.LookPath("dolt")
}

func q43ProcessEnv(home, doltRoot string) []string {
	env := make([]string, 0, len(os.Environ())+12)
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if strings.HasPrefix(key, "BEADS_") || strings.HasPrefix(key, "BD_") || strings.HasPrefix(key, "DOLT_") ||
			key == "HOME" || key == "USERPROFILE" || key == "APPDATA" || key == "XDG_CONFIG_HOME" ||
			key == "GIT_CONFIG_GLOBAL" || key == "GIT_CONFIG_SYSTEM" || key == "GIT_CONFIG_NOSYSTEM" ||
			key == "CGO_ENABLED" || key == "GOFLAGS" || key == "GOTOOLCHAIN" {
			continue
		}
		env = append(env, item)
	}
	return append(env,
		"HOME="+home,
		"USERPROFILE="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, "xdg-config"),
		"DOLT_ROOT_PATH="+doltRoot,
		"DOLT_DISABLE_EVENT_FLUSH=1",
		"GIT_CONFIG_NOSYSTEM=1",
		"BEADS_DOLT_AUTO_START=0",
		"BEADS_NO_DAEMON=1",
		"BD_DISABLE_METRICS=1",
		"BD_DISABLE_EVENT_FLUSH=1",
	)
}

// A private fixture must not inherit a real server/configuration or launch
// Dolt's detached telemetry flusher after its owned server has been waited.
// The executable regressions independently verify the actual child boundary.
func TestQ43ProcessEnvOwnsPrivateTelemetryBoundary(t *testing.T) {
	t.Setenv("DOLT_ROOT_PATH", "/foreign-root-must-not-be-used")
	t.Setenv("DOLT_DISABLE_EVENT_FLUSH", "")
	t.Setenv("BEADS_DOLT_AUTO_START", "1")
	t.Setenv("BD_DISABLE_EVENT_FLUSH", "0")
	home, root := filepath.Join(t.TempDir(), "home"), filepath.Join(t.TempDir(), "dolt-root")
	env := q43ProcessEnv(home, root)
	values, counts := map[string]string{}, map[string]int{}
	for _, item := range env {
		key, value, _ := strings.Cut(item, "=")
		values[key], counts[key] = value, counts[key]+1
	}
	for key, want := range map[string]string{
		"HOME": home, "DOLT_ROOT_PATH": root,
		"DOLT_DISABLE_EVENT_FLUSH": "1", "BEADS_DOLT_AUTO_START": "0",
		"BD_DISABLE_EVENT_FLUSH": "1", "BD_DISABLE_METRICS": "1",
	} {
		if values[key] != want || counts[key] != 1 {
			t.Errorf("private %s = %q (%d copies), want exactly one %q", key, values[key], counts[key], want)
		}
	}
}

func q43SetEnv(env []string, values ...string) []string {
	for _, value := range values {
		key, _, _ := strings.Cut(value, "=")
		filtered := env[:0]
		for _, item := range env {
			got, _, _ := strings.Cut(item, "=")
			if got != key {
				filtered = append(filtered, item)
			}
		}
		env = append(filtered, value)
	}
	return env
}

func q43Run(t *testing.T, timeout time.Duration, env []string, dir, name string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			t.Fatalf("%s %s timed out after %s: %v\nstdout:\n%s\nstderr:\n%s", name, strings.Join(args, " "), timeout, err, stdout.String(), stderr.String())
		}
		t.Fatalf("%s %s failed: %v\nstdout:\n%s\nstderr:\n%s", name, strings.Join(args, " "), err, stdout.String(), stderr.String())
	}
	return stdout.String()
}

func q43BuildBD(t *testing.T, root, home, doltRoot string) string {
	t.Helper()
	if prebuilt := strings.TrimSpace(os.Getenv("BEADS_TEST_BD_BINARY")); prebuilt != "" {
		if _, err := os.Stat(prebuilt); err != nil {
			t.Fatalf("BEADS_TEST_BD_BINARY=%q: %v", prebuilt, err)
		}
		return prebuilt
	}
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate Q43 integration test source")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(source), "../.."))
	if _, err := os.Stat(filepath.Join(repoRoot, "go.mod")); err != nil {
		t.Fatalf("resolve repository root from %s: %v", source, err)
	}
	bin := filepath.Join(root, "bd-nocgo")
	env := q43SetEnv(q43ProcessEnv(home, doltRoot), "CGO_ENABLED=0", "GOFLAGS=", "GOTOOLCHAIN=local")
	q43Run(t, 6*time.Minute, env, repoRoot, "go", "build", "-buildvcs=false", "-p=1", "-tags=gms_pure_go", "-trimpath", "-o", bin, "./cmd/bd")
	return bin
}

func q43UniqueDatabase(t *testing.T) string {
	t.Helper()
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("generate disposable database identity: %v", err)
	}
	return "q43_conformance_" + hex.EncodeToString(suffix[:])
}

func q43FreePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve ephemeral TCP port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("release ephemeral TCP port: %v", err)
	}
	return port
}

type q43LockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *q43LockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *q43LockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

type q43ManagedProcess struct {
	cmd      *exec.Cmd
	done     chan struct{}
	waitErr  error
	stopOnce sync.Once
	stopErr  error
}

func q43StartManagedProcess(t *testing.T, cmd *exec.Cmd, output *q43LockedBuffer, label string, timeout time.Duration) *q43ManagedProcess {
	t.Helper()
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", label, err)
	}
	process := &q43ManagedProcess{cmd: cmd, done: make(chan struct{})}
	go func() {
		process.waitErr = cmd.Wait()
		close(process.done)
	}()
	t.Cleanup(func() {
		if err := process.stop(timeout); err != nil {
			t.Errorf("stop %s: %v", label, err)
		}
	})
	return process
}

func (p *q43ManagedProcess) stop(timeout time.Duration) error {
	p.stopOnce.Do(func() {
		select {
		case <-p.done:
			p.stopErr = p.waitErr
			return
		default:
		}
		if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			select {
			case <-p.done:
				p.stopErr = p.waitErr
				return
			default:
				p.stopErr = fmt.Errorf("send SIGTERM: %w", err)
				return
			}
		}
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-p.done:
			p.stopErr = p.waitErr
		case <-timer.C:
			if err := p.cmd.Process.Kill(); err != nil {
				p.stopErr = fmt.Errorf("process did not stop within %s and kill failed: %w", timeout, err)
				return
			}
			killTimer := time.NewTimer(10 * time.Second)
			defer killTimer.Stop()
			select {
			case <-p.done:
				p.stopErr = fmt.Errorf("process did not stop within %s; killed it", timeout)
			case <-killTimer.C:
				p.stopErr = fmt.Errorf("process did not stop within %s and was not reaped within 10s after kill", timeout)
			}
		}
	})
	return p.stopErr
}

func q43StartDoltServer(t *testing.T, env []string, doltBinary, dataDir string, port int) *q43ManagedProcess {
	t.Helper()
	cmd := exec.Command(doltBinary, "sql-server", "-H", "127.0.0.1", "-P", strconv.Itoa(port), "--no-auto-commit", "--data-dir", dataDir)
	cmd.Env = env
	cmd.Dir = dataDir
	return q43StartManagedProcess(t, cmd, &q43LockedBuffer{}, "disposable Dolt SQL server", 20*time.Second)
}

func q43WaitForDolt(t *testing.T, port int, process *q43ManagedProcess) {
	t.Helper()
	db, err := sql.Open("mysql", fmt.Sprintf("root@tcp(127.0.0.1:%d)/", port))
	if err != nil {
		t.Fatalf("open disposable Dolt readiness connection: %v", err)
	}
	defer db.Close()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-process.done:
			t.Fatalf("disposable Dolt SQL server exited before readiness: %v", process.waitErr)
		default:
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		var version string
		err := db.PingContext(ctx)
		if err == nil {
			err = db.QueryRowContext(ctx, "SELECT DOLT_VERSION()").Scan(&version)
		}
		cancel()
		if err == nil && strings.TrimSpace(version) != "" {
			// A free-port probe cannot transfer ownership of its listener to the
			// child. Bracket the successful Dolt-specific query with liveness
			// checks so a different local MySQL service cannot satisfy readiness
			// after the disposable child loses the bind race.
			select {
			case <-process.done:
				t.Fatalf("disposable Dolt SQL server exited while readiness was being verified: %v", process.waitErr)
			case <-time.After(100 * time.Millisecond):
			}
			select {
			case <-process.done:
				t.Fatalf("disposable Dolt SQL server exited immediately after readiness: %v", process.waitErr)
			default:
				return
			}
		}
		select {
		case <-process.done:
			t.Fatalf("disposable Dolt SQL server exited before readiness: %v", process.waitErr)
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatalf("disposable Dolt SQL server did not accept connections on port %d", port)
}

type q43ServeProcess struct {
	process *q43ManagedProcess
	addr    string
}

func q43StartPrivateProxy(t *testing.T, bd, workspace string, env []string, root string, doltPort int) *q43ManagedProcess {
	t.Helper()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("create private proxy root: %v", err)
	}
	cmd := exec.Command(bd, "db-proxy-child", "--root", root,
		"--port", "0", "--idle-timeout", "-1ns", "--backend", "external",
		"--external-host", "127.0.0.1", "--external-port", strconv.Itoa(doltPort))
	cmd.Dir, cmd.Env = workspace, env
	output := &q43LockedBuffer{}
	process := q43StartManagedProcess(t, cmd, output, "private SQL proxy", 60*time.Second)
	deadline := time.NewTimer(90 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if running, pid := proxy.IsRunning(root); running {
			if pid != cmd.Process.Pid {
				t.Fatalf("private proxy discovery names pid %d, want owned pid %d", pid, cmd.Process.Pid)
			}
			q43AssertOwnedProxyRunning(t, root, process)
			return process
		}
		select {
		case <-process.done:
			t.Fatalf("private proxy exited before readiness: %v\n%s", process.waitErr, output.String())
		case <-deadline.C:
			t.Fatalf("private proxy did not become ready within 90s\n%s", output.String())
		case <-ticker.C:
		}
	}
}

func q43AssertOwnedProxyRunning(t *testing.T, root string, process *q43ManagedProcess) {
	t.Helper()
	select {
	case <-process.done:
		t.Fatalf("owned private proxy exited early: %v", process.waitErr)
	default:
	}
	if running, pid := proxy.IsRunning(root); !running || pid != process.cmd.Process.Pid {
		t.Fatalf("private proxy discovery = (%v, %d), want owned running pid %d", running, pid, process.cmd.Process.Pid)
	}
}

func q43StartServe(t *testing.T, bd, workspace string, env []string, tokenFile string) *q43ServeProcess {
	t.Helper()
	cmd := exec.Command(bd, "serve", "--addr", "127.0.0.1:0", "--auth-token-file", tokenFile)
	cmd.Dir, cmd.Env = workspace, env
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("capture bd serve address: %v", err)
	}
	stderr := &q43LockedBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start disposable bd serve: %v", err)
	}
	process := &q43ManagedProcess{cmd: cmd, done: make(chan struct{})}
	go func() {
		process.waitErr = cmd.Wait()
		close(process.done)
	}()
	sp := &q43ServeProcess{process: process}
	t.Cleanup(func() {
		if err := process.stop(60 * time.Second); err != nil {
			t.Errorf("stop disposable bd serve: %v\nstderr:\n%s", err, stderr.String())
		}
	})

	lines := make(chan string, 8)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			default:
			}
		}
		close(lines)
	}()
	deadline := time.NewTimer(90 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("bd serve exited before announcing its bound address: %v\nstderr:\n%s", process.waitErr, stderr.String())
			}
			addr, found := strings.CutPrefix(strings.TrimSpace(line), "bd serve: listening on http://")
			if found {
				sp.addr = addr
				return sp
			}
		case <-process.done:
			t.Fatalf("bd serve exited before announcing its bound address: %v\nstderr:\n%s", process.waitErr, stderr.String())
		case <-deadline.C:
			t.Fatalf("bd serve did not announce its bound address within 90s\nstderr:\n%s", stderr.String())
		}
	}
}

func (sp *q43ServeProcess) baseURL() string { return "http://" + sp.addr }

func (sp *q43ServeProcess) stop() error {
	return sp.process.stop(60 * time.Second)
}

func q43Client(t *testing.T, baseURL, projectID, database string, client *http.Client) *httpclient.Client {
	t.Helper()
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	result, err := httpclient.New(httpclient.Config{
		BaseURL: baseURL, Token: q43IntegrationToken, ProjectID: projectID, Database: database, HTTPClient: client,
	})
	if err != nil {
		t.Fatalf("create production Q43 HTTP client: %v", err)
	}
	return result
}

func q43AssertReadyContext(t *testing.T, client *httpclient.Client, baseURL, projectID, database string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := client.VerifyRevisionTransition(ctx, projectID, database); err != nil {
		t.Fatalf("verify live server transition identity and capabilities: %v", err)
	}
	if err := client.VerifySourceSnapshot(ctx, projectID, database); err != nil {
		t.Fatalf("verify live server source-snapshot identity and capabilities: %v", err)
	}
	response, err := q43AuthenticatedGET(ctx, baseURL+"/v0/beads/context", projectID)
	if err != nil {
		t.Fatalf("read authenticated server context: %v", err)
	}
	var body struct {
		APIVersion   string   `json:"api_version"`
		ProjectID    string   `json:"project_id"`
		Database     string   `json:"database"`
		DoltMode     string   `json:"dolt_mode"`
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(response, &body); err != nil {
		t.Fatalf("decode live server context: %v", err)
	}
	if body.APIVersion != "v0" || body.ProjectID != projectID || body.Database != database || body.DoltMode != "server" {
		t.Fatalf("server identity = api %q project %q database %q mode %q, want v0, %q, %q, server", body.APIVersion, body.ProjectID, body.Database, body.DoltMode, projectID, database)
	}
	for _, required := range []string{"project.enforce", "issues.transitionMetadata", "issues.transitionReceipt.get", "issues.sourceSnapshot"} {
		if !slicesContains(body.Capabilities, required) {
			t.Fatalf("server context omits Q43 capability %q: %v", required, body.Capabilities)
		}
	}
}

func slicesContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func q43AuthenticatedGET(ctx context.Context, endpoint, projectID string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+q43IntegrationToken)
	request.Header.Set("Bd-Project-Id", projectID)
	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET returned HTTP %d: %s", response.StatusCode, body)
	}
	return body, nil
}

type q43IssueSnapshot struct {
	Revision int64
	Metadata map[string]json.RawMessage
}

func q43ReadIssue(t *testing.T, baseURL, projectID, issueID string) q43IssueSnapshot {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	body, err := q43AuthenticatedGET(ctx, baseURL+"/v0/beads/issues/"+url.PathEscape(issueID), projectID)
	if err != nil {
		t.Fatalf("read issue %s revision: %v", issueID, err)
	}
	var issue struct {
		ID       string          `json:"id"`
		Revision string          `json:"revision"`
		Metadata json.RawMessage `json:"metadata"`
	}
	if err := json.Unmarshal(body, &issue); err != nil {
		t.Fatalf("decode issue %s: %v", issueID, err)
	}
	if issue.ID != issueID || issue.Revision == "" {
		t.Fatalf("issue response identity/revision = %q/%q, want %q/nonempty", issue.ID, issue.Revision, issueID)
	}
	revision, err := strconv.ParseInt(issue.Revision, 10, 64)
	if err != nil || revision == 0 {
		t.Fatalf("issue revision %q is not a nonzero signed int64: %v", issue.Revision, err)
	}
	metadata := make(map[string]json.RawMessage)
	if len(issue.Metadata) > 0 {
		if err := json.Unmarshal(issue.Metadata, &metadata); err != nil {
			t.Fatalf("decode issue %s metadata: %v", issueID, err)
		}
	}
	return q43IssueSnapshot{Revision: revision, Metadata: metadata}
}

func q43IssueRevision(t *testing.T, baseURL, projectID, issueID string) int64 {
	t.Helper()
	return q43ReadIssue(t, baseURL, projectID, issueID).Revision
}

func q43AssertSourceSnapshot(t *testing.T, client *httpclient.Client, baseURL, projectID, issueID string, expectedRevision int64, expectedEdges map[string]string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	snapshot, err := client.GetSourceSnapshot(ctx, issueID)
	if err != nil {
		t.Fatalf("read source snapshot for %s: %v", issueID, err)
	}
	if snapshot == nil || snapshot.Issue.ID != issueID {
		t.Fatalf("source snapshot issue = %+v, want %s", snapshot, issueID)
	}
	wantToken := strconv.FormatInt(expectedRevision, 10)
	if snapshot.Issue.RowVersion != expectedRevision || snapshot.Issue.Revision != wantToken {
		t.Fatalf("source snapshot revision = %d/%q, want %d/%q", snapshot.Issue.RowVersion, snapshot.Issue.Revision, expectedRevision, wantToken)
	}
	if liveRevision := q43IssueRevision(t, baseURL, projectID, issueID); liveRevision != expectedRevision {
		t.Fatalf("live issue revision = %d, want source snapshot revision %d", liveRevision, expectedRevision)
	}
	if snapshot.OutgoingDependencies == nil {
		t.Fatal("source snapshot dependency list is nil, want explicit empty or complete array")
	}
	if len(snapshot.OutgoingDependencies) != len(expectedEdges) {
		t.Fatalf("source snapshot edges = %+v, want %d exact edges", snapshot.OutgoingDependencies, len(expectedEdges))
	}
	seen := make(map[string]bool, len(snapshot.OutgoingDependencies))
	for _, edge := range snapshot.OutgoingDependencies {
		if edge == nil || edge.IssueID != issueID {
			t.Fatalf("source snapshot contains invalid outgoing edge: %+v", edge)
		}
		wantType, ok := expectedEdges[edge.DependsOnID]
		if !ok || string(edge.Type) != wantType || seen[edge.DependsOnID] {
			t.Fatalf("source snapshot contains unexpected or duplicate edge: %+v (expected %v)", edge, expectedEdges)
		}
		seen[edge.DependsOnID] = true
	}
}

func q43AssertIssueEffect(t *testing.T, baseURL, projectID, issueID string, request httpclient.TransitionMetadataRequest) int64 {
	t.Helper()
	if request.Value == nil {
		t.Fatal("transition request value is nil")
	}
	issue := q43ReadIssue(t, baseURL, projectID, issueID)
	value, found := issue.Metadata[request.Key]
	if !found || !bytes.Equal(value, *request.Value) {
		t.Fatalf("issue %s metadata[%q] = %s (found=%v), want %s", issueID, request.Key, value, found, *request.Value)
	}
	return issue.Revision
}

func q43TransitionRequest(receiptID, value string, expectedVersion int64) httpclient.TransitionMetadataRequest {
	encodedValue := json.RawMessage(strconv.Quote(value))
	return httpclient.TransitionMetadataRequest{
		ReceiptID:       receiptID,
		Scope:           "q43-server-mode-conformance",
		Kind:            "set-marker",
		Actor:           "q43-integration",
		ExpectedVersion: expectedVersion,
		Key:             q43IssueMetadataKey,
		Value:           &encodedValue,
		Payload:         json.RawMessage(`{"suite":"q43-server-mode"}`),
	}
}

func q43AssertReceipt(t *testing.T, receipt *httpclient.TransitionReceipt, issueID string, request httpclient.TransitionMetadataRequest) {
	t.Helper()
	if receipt == nil {
		t.Fatal("transition receipt is nil")
	}
	if request.Value == nil {
		t.Fatal("transition request value is nil")
	}
	expectedMatches := request.Expected == nil && receipt.Expected == nil
	if request.Expected != nil {
		expectedMatches = bytes.Equal(receipt.Expected, *request.Expected)
	}
	if receipt.ReceiptID != request.ReceiptID || receipt.IssueID != issueID || receipt.Scope != request.Scope ||
		receipt.Kind != request.Kind || receipt.Actor != request.Actor || receipt.ExpectedVersion != request.ExpectedVersion ||
		receipt.ToVersion == 0 || receipt.ToVersion == request.ExpectedVersion || receipt.Key != request.Key ||
		!expectedMatches || !bytes.Equal(receipt.Value, *request.Value) || !bytes.Equal(receipt.Payload, request.Payload) {
		t.Fatalf("receipt = %+v, does not match request for %s", receipt, issueID)
	}
}

func q43AssertConcurrentReplay(t *testing.T, client *httpclient.Client, issueID string, request httpclient.TransitionMetadataRequest) int64 {
	t.Helper()
	start := make(chan struct{})
	results := make(chan struct {
		result httpclient.TransitionMetadataResult
		err    error
	}, 2)
	for range 2 {
		go func() {
			<-start
			ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
			defer cancel()
			result, err := client.TransitionMetadata(ctx, issueID, request)
			results <- struct {
				result httpclient.TransitionMetadataResult
				err    error
			}{result: result, err: err}
		}()
	}
	close(start)
	var applied, replayed int
	var toVersion int64
	for range 2 {
		outcome := <-results
		if outcome.err != nil {
			t.Fatalf("concurrent exact transition: %v", outcome.err)
		}
		if !outcome.result.Applied || outcome.result.Receipt == nil {
			t.Fatalf("concurrent transition result = %+v, want applied receipt", outcome.result)
		}
		q43AssertReceipt(t, outcome.result.Receipt, issueID, request)
		if toVersion == 0 {
			toVersion = outcome.result.Receipt.ToVersion
		} else if toVersion != outcome.result.Receipt.ToVersion {
			t.Fatalf("concurrent replies carry different resulting revisions: %d and %d", toVersion, outcome.result.Receipt.ToVersion)
		}
		if outcome.result.Replayed {
			replayed++
		} else {
			applied++
		}
	}
	if applied != 1 || replayed != 1 {
		t.Fatalf("concurrent results = %d first applies, %d replays; want one of each", applied, replayed)
	}
	return toVersion
}

func q43ConcurrentTransitionProxy(t *testing.T, target, receiptID string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	upstream, err := url.Parse(target)
	if err != nil {
		t.Fatalf("parse Q43 server URL: %v", err)
	}
	transport := &http.Transport{DisableKeepAlives: true, MaxConnsPerHost: 2}
	t.Cleanup(transport.CloseIdleConnections)
	arrivals := &atomic.Int32{}
	release := make(chan struct{})
	var releaseOnce sync.Once
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var transitionBody []byte
		isTransitionPost := r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, ":transitionMetadata")
		if isTransitionPost {
			var readErr error
			transitionBody, readErr = io.ReadAll(io.LimitReader(r.Body, 1<<20))
			if readErr != nil {
				http.Error(w, "concurrency proxy could not read request", http.StatusBadRequest)
				return
			}
			var request struct {
				ReceiptID string `json:"receipt_id"`
			}
			if err := json.Unmarshal(transitionBody, &request); err != nil || request.ReceiptID != receiptID {
				http.Error(w, "concurrency proxy received the wrong transition", http.StatusBadRequest)
				return
			}
			arrival := arrivals.Add(1)
			if arrival == 2 {
				releaseOnce.Do(func() { close(release) })
			} else if arrival > 2 {
				http.Error(w, "concurrency proxy received more than two transitions", http.StatusConflict)
				return
			}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			case <-time.After(15 * time.Second):
				http.Error(w, "concurrent transition peer did not arrive", http.StatusGatewayTimeout)
				return
			}
		}

		upstreamURL := *upstream
		upstreamURL.Path = r.URL.Path
		upstreamURL.RawPath = r.URL.RawPath
		upstreamURL.RawQuery = r.URL.RawQuery
		forward := r.Clone(r.Context())
		forward.URL = &upstreamURL
		forward.RequestURI = ""
		forward.Host = upstreamURL.Host
		if isTransitionPost {
			forward.Body = io.NopCloser(bytes.NewReader(transitionBody))
			forward.ContentLength = int64(len(transitionBody))
		}
		response, err := transport.RoundTrip(forward)
		if err != nil {
			http.Error(w, "concurrency proxy upstream request failed", http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		for name, values := range response.Header {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server, arrivals
}

func q43LostResponseProxy(t *testing.T, target string) (*httptest.Server, *atomic.Bool, <-chan error) {
	t.Helper()
	upstream, err := url.Parse(target)
	if err != nil {
		t.Fatalf("parse Q43 server URL: %v", err)
	}
	transport := &http.Transport{DisableKeepAlives: true}
	t.Cleanup(transport.CloseIdleConnections)
	dropped := &atomic.Bool{}
	dropErr := make(chan error, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamURL := *upstream
		upstreamURL.Path = r.URL.Path
		upstreamURL.RawPath = r.URL.RawPath
		upstreamURL.RawQuery = r.URL.RawQuery
		forward := r.Clone(r.Context())
		forward.URL = &upstreamURL
		forward.RequestURI = ""
		forward.Host = upstreamURL.Host
		response, err := transport.RoundTrip(forward)
		if err != nil {
			http.Error(w, "fault proxy upstream request failed", http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		isTransitionPost := r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, ":transitionMetadata")
		if isTransitionPost && response.StatusCode == http.StatusOK && dropped.CompareAndSwap(false, true) {
			if _, err := io.Copy(io.Discard, response.Body); err != nil {
				dropErr <- fmt.Errorf("read successful upstream transition response: %w", err)
				return
			}
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				dropped.Store(false)
				dropErr <- errors.New("fault proxy response writer does not support connection hijacking")
				http.Error(w, "fault proxy could not drop response", http.StatusInternalServerError)
				return
			}
			connection, _, err := hijacker.Hijack()
			if err != nil {
				dropped.Store(false)
				dropErr <- fmt.Errorf("hijack client connection: %w", err)
				return
			}
			_ = connection.Close()
			return
		}
		for name, values := range response.Header {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		w.WriteHeader(response.StatusCode)
		if _, err := io.Copy(w, response.Body); err != nil {
			dropErr <- fmt.Errorf("forward upstream HTTP response: %w", err)
		}
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server, dropped, dropErr
}
