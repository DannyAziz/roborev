package prompt

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/testutil"
)

// TestPrepareRefusesSymlinkedSnapshotRoot is the hostile-repo pin for the
// write side. A .roborev that is a symlink out of the repository is refused,
// whether the repository ships it or it is swapped in after validation.
// Nothing is written outside the repository.
func TestPrepareRefusesSymlinkedSnapshotRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows due to symlink semantics")
	}
	for _, tc := range []struct {
		name    string
		swapped bool
	}{
		{name: "committed symlink"},
		{name: "swapped after validation", swapped: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := testutil.NewTestRepoWithCommit(t).Path()
			outside := t.TempDir()
			link := func() {
				require.NoError(t, os.RemoveAll(filepath.Join(repo, ".roborev")))
				require.NoError(t, os.Symlink(outside, filepath.Join(repo, ".roborev")))
			}
			if tc.swapped {
				snapshotBeforeCreateForTest = link
				t.Cleanup(func() { snapshotBeforeCreateForTest = nil })
			} else {
				link()
			}
			builder := NewBuilderWithConfig(nil, &config.Config{DefaultMaxPromptSize: 32}).ForRepo(repo, 0)
			prepared, err := builder.Prepare(strings.Repeat("oversized prompt line\n", 20), SnapshotTarget{})
			if prepared.Cleanup != nil {
				t.Cleanup(prepared.Cleanup)
			}
			require.Error(t, err)
			assert.Empty(t, prepared.FilePath)
			entries, err := os.ReadDir(outside)
			require.NoError(t, err)
			assert.Empty(t, entries)
		})
	}
}
