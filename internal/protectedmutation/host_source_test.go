package protectedmutation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestFileHostAuthoritySourceLoadsColocatedFilesAndPinsBindingConfig(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	config := HostAuthorityConfig{
		SchemaVersion:   HostAuthoritySchemaV1,
		Audience:        "audience-a",
		ProjectID:       "project-a",
		Database:        "database-a",
		KeyringFile:     "keys.json",
		RevocationsFile: "revocations.json",
	}
	writeHostJSON(t, filepath.Join(directory, "authority.json"), config)
	keyring := Keyring{SchemaVersion: HostAuthoritySchemaV1, Generation: 7, Keys: []TrustedKey{}}
	writeHostJSON(t, filepath.Join(directory, config.KeyringFile), keyring)
	signed := SignedRevocations{Payload: json.RawMessage(`{"signed":"snapshot"}`), Signature: "signature"}
	writeHostJSON(t, filepath.Join(directory, config.RevocationsFile), signed)

	source, gotConfig, err := NewFileHostAuthoritySource(filepath.Join(directory, "authority.json"))
	if err != nil {
		t.Fatalf("NewFileHostAuthoritySource: %v", err)
	}
	defer source.Close()
	if gotConfig != config {
		t.Fatalf("config = %#v, want %#v", gotConfig, config)
	}
	bundle, err := source.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if bundle.Generation != keyring.Generation || len(bundle.Keys) != 0 || string(bundle.Revocations.Payload) != string(signed.Payload) {
		t.Fatalf("bundle = %#v, want current files", bundle)
	}
}

func TestFileHostAuthorityRejectsDuplicateMembersAndUnprotectedFiles(t *testing.T) {
	t.Run("duplicate configuration member", func(t *testing.T) {
		directory := protectedAuthorityTestDir(t)
		path := filepath.Join(directory, "authority.json")
		writeHostBytes(t, path, []byte(`{"schema_version":1,"schema_version":1,"audience":"audience-a","project_id":"project-a","database":"database-a","keyring_file":"keys.json","revocations_file":"revocations.json"}`))
		if _, _, err := NewFileHostAuthoritySource(path); err == nil {
			t.Fatal("NewFileHostAuthoritySource accepted duplicate JSON members")
		}
	})

	t.Run("duplicate keyring member", func(t *testing.T) {
		directory := protectedAuthorityTestDir(t)
		config := writeValidHostFiles(t, directory)
		writeHostBytes(t, filepath.Join(directory, config.KeyringFile), []byte(`{"schema_version":1,"schema_version":1,"generation":1,"keys":[]}`))
		source, _, err := NewFileHostAuthoritySource(filepath.Join(directory, "authority.json"))
		if err != nil {
			t.Fatalf("open config: %v", err)
		}
		defer source.Close()
		if _, err := source.Load(context.Background()); err == nil {
			t.Fatal("Load accepted a duplicate keyring member")
		}
	})

	t.Run("duplicate signed envelope member", func(t *testing.T) {
		directory := protectedAuthorityTestDir(t)
		config := writeValidHostFiles(t, directory)
		writeHostBytes(t, filepath.Join(directory, config.RevocationsFile), []byte(`{"payload":{},"payload":{},"signature":"signature"}`))
		source, _, err := NewFileHostAuthoritySource(filepath.Join(directory, "authority.json"))
		if err != nil {
			t.Fatalf("open config: %v", err)
		}
		defer source.Close()
		if _, err := source.Load(context.Background()); err == nil {
			t.Fatal("Load accepted duplicate envelope members")
		}
	})

	t.Run("world writable keyring", func(t *testing.T) {
		directory := protectedAuthorityTestDir(t)
		config := writeValidHostFiles(t, directory)
		keyringPath := filepath.Join(directory, config.KeyringFile)
		if err := os.Chmod(keyringPath, 0o666); err != nil {
			t.Fatal(err)
		}
		source, _, err := NewFileHostAuthoritySource(filepath.Join(directory, "authority.json"))
		if err != nil {
			t.Fatalf("open config: %v", err)
		}
		defer source.Close()
		if _, err := source.Load(context.Background()); err == nil {
			t.Fatal("Load accepted a world-writable keyring")
		}
	})

	t.Run("symlinked revocation file", func(t *testing.T) {
		directory := protectedAuthorityTestDir(t)
		config := writeValidHostFiles(t, directory)
		if err := os.Remove(filepath.Join(directory, config.RevocationsFile)); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(t.TempDir(), "revocations.json")
		writeHostJSON(t, outside, SignedRevocations{Payload: json.RawMessage(`{}`), Signature: "signature"})
		if err := os.Symlink(outside, filepath.Join(directory, config.RevocationsFile)); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		source, _, err := NewFileHostAuthoritySource(filepath.Join(directory, "authority.json"))
		if err != nil {
			t.Fatalf("open config: %v", err)
		}
		defer source.Close()
		if _, err := source.Load(context.Background()); err == nil {
			t.Fatal("Load accepted a symlinked revocation file")
		}
	})
}

func TestFileHostAuthorityRejectsPathNamesOutsidePinnedDirectory(t *testing.T) {
	for _, name := range []string{"../keys.json", "/tmp/keys.json", "sub/keys.json", ".."} {
		t.Run(name, func(t *testing.T) {
			directory := protectedAuthorityTestDir(t)
			config := HostAuthorityConfig{
				SchemaVersion: HostAuthoritySchemaV1, Audience: "audience-a", ProjectID: "project-a", Database: "database-a",
				KeyringFile: name, RevocationsFile: "revocations.json",
			}
			writeHostJSON(t, filepath.Join(directory, "authority.json"), config)
			if _, _, err := NewFileHostAuthoritySource(filepath.Join(directory, "authority.json")); err == nil {
				t.Fatalf("accepted unsafe keyring file name %q", name)
			}
		})
	}
}

func protectedAuthorityTestDir(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	return directory
}

func writeValidHostFiles(t *testing.T, directory string) HostAuthorityConfig {
	t.Helper()
	config := HostAuthorityConfig{
		SchemaVersion: HostAuthoritySchemaV1, Audience: "audience-a", ProjectID: "project-a", Database: "database-a",
		KeyringFile: "keys.json", RevocationsFile: "revocations.json",
	}
	writeHostJSON(t, filepath.Join(directory, "authority.json"), config)
	writeHostJSON(t, filepath.Join(directory, config.KeyringFile), Keyring{SchemaVersion: HostAuthoritySchemaV1, Generation: 1, Keys: []TrustedKey{}})
	writeHostJSON(t, filepath.Join(directory, config.RevocationsFile), SignedRevocations{Payload: json.RawMessage(`{}`), Signature: "signature"})
	return config
}

func writeHostJSON(t *testing.T, path string, value any) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	writeHostBytes(t, path, encoded)
}

func writeHostBytes(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
