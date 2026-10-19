//go:build integration && !windows

package main

import (
	"fmt"
	"strings"
	"testing"
)

// Exercise the printed CLI recipe, not a duplicate command formatter. A user
// following a guarded preview must retain that guard if the graph changes.
func TestDeletePreviewPreservesActiveReferenceGuard(t *testing.T) {
	f := newRevisionCLIFixture(t)
	f.env = append(f.env, "NO_COLOR=1")
	for _, mode := range []struct {
		name    string
		count   int
		cascade bool
	}{
		{name: "single", count: 1},
		{name: "batch", count: 2},
		{name: "cascade", count: 1, cascade: true},
	} {
		for _, guarded := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/guarded=%t", mode.name, guarded), func(t *testing.T) {
				ids := make([]string, mode.count)
				for i := range ids {
					ids[i] = fmt.Sprintf("rvcli-hint-%s-%t-%d", mode.name, guarded, i)
					f.create(t, ids[i])
					f.ok(t, "close", ids[i], "--json")
				}
				preview := append([]string{"delete"}, ids...)
				if mode.cascade {
					preview = append(preview, "--cascade")
				}
				if guarded {
					preview = append(preview, "--protect-active-references")
				}
				out := f.ok(t, preview...)
				var commands [][]string
				for _, line := range strings.Split(string(out), "\n") {
					if strings.Contains(line, "To proceed") {
						start := strings.Index(line, "bd delete ")
						if start < 0 {
							t.Fatalf("confirmation has no bd delete command: %s", line)
						}
						commands = append(commands, strings.Fields(line[start:]))
					}
				}
				want := append([]string{"bd", "delete"}, ids...)
				if mode.cascade {
					want = append(want, "--cascade")
				}
				want = append(want, "--force")
				if guarded {
					want = append(want, "--protect-active-references")
				}
				if len(commands) != 1 || strings.Join(commands[0], " ") != strings.Join(want, " ") {
					t.Fatalf("printed confirmation commands=%q, want one %q\n%s", commands, want, out)
				}
				for _, id := range ids {
					if got := f.row(t, id); got.Status != "closed" {
						t.Fatalf("preview changed target: %+v", got)
					}
				}
				if !guarded || mode.cascade {
					// Cascade deliberately deletes its resolved owners; the
					// surviving-owner race belongs to single/batch execution.
					return
				}
				owner := fmt.Sprintf("rvcli-hint-owner-%s", mode.name)
				f.create(t, owner)
				f.ok(t, "dep", "add", owner, ids[0], "--type", "relates-to")
				targetBefore, ownerBefore := f.row(t, ids[0]), f.row(t, owner)
				refusal, code := f.run(t, commands[0][1:]...)
				if code == 0 || !strings.Contains(string(refusal), "surviving reference owner") {
					t.Fatalf("printed command lost active-owner protection: exit=%d\n%s", code, refusal)
				}
				f.unchanged(t, ids[0], targetBefore)
				f.unchanged(t, owner, ownerBefore)
			})
		}
	}
}
