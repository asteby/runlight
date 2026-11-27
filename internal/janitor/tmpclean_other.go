//go:build !linux

package janitor

import (
	"errors"
	"io/fs"
)

func fileOwner(fs.FileInfo) (uint32, bool) { return 0, false }

func resolveOwner(string) (uint32, error) {
	return 0, errors.New("owner filter needs Linux")
}

// Without /proc we cannot know what is open, so the cleaner refuses to run.
func scanOpenEntries([]string) (map[string]bool, error) {
	return nil, errors.New("open-file scan needs Linux /proc")
}
