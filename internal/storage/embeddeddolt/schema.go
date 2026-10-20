//go:build cgo

package embeddeddolt

import (
	"github.com/rhar1511/beads/internal/storage/schema"
)

func LatestVersion() int {
	return schema.LatestVersion()
}

func LatestIgnoredVersion() int {
	return schema.LatestIgnoredVersion()
}
