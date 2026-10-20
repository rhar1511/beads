package main

import (
	"path/filepath"

	"github.com/rhar1511/beads/internal/config"
)

const defaultImportJSONLPath = "issues.jsonl"

func configuredImportJSONLPath(beadsDir string) string {
	importPath := config.GetString("import.path")
	if importPath == "" {
		importPath = defaultImportJSONLPath
	}
	return filepath.Join(beadsDir, importPath)
}
