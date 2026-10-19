package main

import (
	"errors"
	"testing"

	"github.com/spf13/cobra"
)

func TestRevisionCASProxyRefusedBeforeProvider(t *testing.T) {
	for _, topology := range []ProxyTopology{ProxyTopologyManagedLocal, ProxyTopologyExternalTCP, ProxyTopologyExternalUnix, ProxyTopologyTeamServer, ProxyTopologyUnknown} {
		t.Run(string(topology), func(t *testing.T) {
			cmd := &cobra.Command{Use: "update"}
			registerRevisionPrecondition(cmd)
			if err := cmd.Flags().Set("if-revision", "0"); err != nil {
				t.Fatal(err)
			}
			err := validateProxyRegistryBeforeProvider(cmd, topology)
			var exit *exitError
			if !errors.As(err, &exit) || exit.Code != revisionUnsupportedExit {
				t.Fatalf("proxy conditional write error = %v, want unsupported exit %d", err, revisionUnsupportedExit)
			}
		})
	}
}

// Gas City discovers conditional-write support through all four public help
// surfaces. Advertising only update would leave lifecycle dispatch incapable.
func TestRevisionCASFlagsPublicContract(t *testing.T) {
	for _, cmd := range []*cobra.Command{updateCmd, closeCmd, assignCmd, deleteCmd} {
		t.Run(cmd.Name(), func(t *testing.T) {
			flag := cmd.Flags().Lookup("if-revision")
			if flag == nil {
				t.Fatal("ordinary lifecycle CLI lacks --if-revision; conditional writes unsupported")
			}
			if flag.Value.Type() != "int64" {
				t.Fatalf("revision token type = %s, want lossless signed int64", flag.Value.Type())
			}
		})
	}
}

// Labels and ancestry belong to the same atomic lifecycle patch as row fields.
func TestRevisionCASAllowsAggregatePatchArguments(t *testing.T) {
	for _, flag := range []string{"add-label", "remove-label", "set-labels", "parent"} {
		t.Run(flag, func(t *testing.T) {
			cmd := &cobra.Command{Use: "update"}
			registerRevisionPrecondition(cmd)
			cmd.Flags().Bool("claim", false, "")
			for _, name := range []string{"add-label", "remove-label", "set-labels", "parent"} {
				cmd.Flags().String(name, "", "")
			}
			if err := cmd.Flags().Set("if-revision", "123"); err != nil {
				t.Fatal(err)
			}
			if err := cmd.Flags().Set(flag, "changed"); err != nil {
				t.Fatal(err)
			}
			if err := validateRevisionArguments(cmd, []string{"bd-target"}, 1); err != nil {
				t.Fatalf("atomic %s patch refused: %v", flag, err)
			}
		})
	}
}
