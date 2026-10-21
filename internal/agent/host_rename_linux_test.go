//go:build linux

package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// A filesystem that rejects renameat2(RENAME_NOREPLACE) must not fall back to
// a replacing rename. Replacing renameHandoffFile with os.Rename fails
// TestWriteSyncedTempRefusesExistingFile; this covers the unsupported-flag
// error that would take a fallback.
func TestWriteSyncedTempUnsupportedRenameat2DoesNotReplace(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "prompt.md")
	require.NoError(t, os.WriteFile(final, []byte("planted"), 0o600))
	prev := renameHandoffFile
	renameHandoffFile = func(oldpath, newpath string) error {
		return &os.LinkError{Op: "renameat2", Old: oldpath, New: newpath, Err: unix.ENOTSUP}
	}
	t.Cleanup(func() { renameHandoffFile = prev })

	err := writeSyncedTemp(dir, "prompt.md", []byte("overwrite"), final)

	require.Error(t, err)
	var linkErr *os.LinkError
	require.ErrorAs(t, err, &linkErr)
	assert.Equal(t, "renameat2", linkErr.Op)
	require.ErrorIs(t, err, unix.ENOTSUP)
	assert.Equal(t, "planted", readFileString(final))
	entries, readErr := os.ReadDir(dir)
	require.NoError(t, readErr)
	require.Len(t, entries, 1)
	assert.Equal(t, "prompt.md", entries[0].Name())
}
