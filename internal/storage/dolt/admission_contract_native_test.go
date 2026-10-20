//go:build integration && !windows

package dolt

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rhar1511/beads/internal/doltserver"
	"github.com/rhar1511/beads/internal/testutil/integration"
)

// TestAdmissionContractNative runs the store adapter contract without Docker,
// using a private server whose port and lifetime belong to this test alone.
func TestAdmissionContractNative(t *testing.T) {
	integration.RequireDolt(t)
	t.Setenv("BEADS_DOLT_SHARED_SERVER", "0")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "")
	t.Setenv("BEADS_DOLT_PORT", "")
	beadsDir := filepath.Join(t.TempDir(), ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	state, err := doltserver.Start(beadsDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := doltserver.Stop(beadsDir); err != nil {
			t.Errorf("stop owned Dolt: %v", err)
		}
	})
	ctx := t.Context()
	store, err := New(ctx, &Config{Path: filepath.Join(beadsDir, "store"), BeadsDir: beadsDir, ServerHost: "127.0.0.1", ServerPort: state.Port, ServerUser: "root", Database: "test_admission_native", CreateIfMissing: true, MaxOpenConns: 1, CommitterName: "Admission Contract", CommitterEmail: "contract@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	if err := store.SetConfig(ctx, "issue_prefix", "adm"); err != nil {
		t.Fatal(err)
	}
	runDoltAdmissionContract(t, ctx, store)
}
