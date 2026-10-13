package scripts_test

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestNightlyCoverageGateUsesOneAggregateAndFailsClosed(t *testing.T) {
	step := readCIWorkflow(t, "nightly.yml").job(t, "full-test").step(t, "Check coverage threshold")
	for _, tc := range []struct {
		name       string
		summary    string
		toolStatus string
		wantExit   int
		wantOutput string
	}{
		{"function named total below threshold", "file.go:1: total 0.0%\ntotal: (statements) 20.0%\n", "0", 1, "Coverage: 20.0%"},
		{"observed hosted coverage", "file.go:1: total 0.0%\ntotal: (statements) 40.5%\n", "0", 0, "Coverage: 40.5%"},
		{"threshold boundary", "total: (statements) 30.0%\n", "0", 0, "Coverage: 30.0%"},
		{"below threshold", "total: (statements) 29.9%\n", "0", 1, "Coverage: 29.9%"},
		{"missing aggregate", "file.go:1: total 99.0%\n", "0", 1, "Invalid or ambiguous aggregate coverage"},
		{"duplicate aggregate", "total: (statements) 40.5%\ntotal: (statements) 50.0%\n", "0", 1, "Invalid or ambiguous aggregate coverage"},
		{"malformed aggregate", "total: (statements) NaN%\n", "0", 1, "Invalid or ambiguous aggregate coverage"},
		{"failed coverage tool", "total: (statements) 99.0%\n", "1", 1, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Exercise the workflow's actual shell step while substituting only
			// go tool cover's output and exit status.
			script := `go() { printf '%s' "$COVERAGE_FIXTURE"; return "$COVERAGE_TOOL_STATUS"; }
` + step.Run
			cmd := exec.CommandContext(t.Context(), "bash", "--noprofile", "--norc", "-e", "-c", script)
			cmd.Env = append(os.Environ(), "COVERAGE_FIXTURE="+tc.summary, "COVERAGE_TOOL_STATUS="+tc.toolStatus)
			output, err := cmd.CombinedOutput()
			gotExit := 0
			if err != nil {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) {
					t.Fatalf("run coverage step: %v", err)
				}
				gotExit = exitErr.ExitCode()
			}
			if gotExit != tc.wantExit || !strings.Contains(string(output), tc.wantOutput) {
				t.Fatalf("coverage gate exit=%d output=%s; want exit=%d containing %q", gotExit, output, tc.wantExit, tc.wantOutput)
			}
		})
	}
}
