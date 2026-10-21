package prompt

import (
	"strconv"
	"strings"
	"sync"

	"go.kenn.io/roborev/internal/config"
)

// preparedPrompts maps each handoff Prepare returned in this process to the
// complete text it wrote, until that snapshot's cleanup runs.
var preparedPrompts sync.Map

// Prepare measures the complete prompt once content assembly is finished.
// The configured threshold selects inline text or a complete prompt file;
// neither path drops instructions, discussion, diffs, or review results.
func (b *Builder) Prepare(text string, target SnapshotTarget) (SnapshotResult, error) {
	// Diffs may contain legacy-encoded bytes. Normalize the assembled prompt
	// before measuring it; repository contents are left untouched.
	text = strings.ToValidUTF8(text, "\uFFFD")
	configRepo := target.ConfigRepoPath
	if configRepo == "" {
		configRepo = b.resolvedConfigRepoPath()
	}
	if len(text) <= config.ResolveMaxPromptSize(configRepo, b.globalCfg) {
		return SnapshotResult{Prompt: text}, nil
	}
	repo, root, err := b.resolveSnapshotTarget(target)
	if err != nil {
		return SnapshotResult{}, err
	}
	file, cleanup, err := writeExternalSnapshot(repo, root, "prompt.md", text)
	if err != nil {
		return SnapshotResult{}, err
	}
	handoff := SnapshotHandoffPrompt(file)
	preparedPrompts.Store(handoff, text)
	return SnapshotResult{
		Prompt:   handoff,
		FilePath: file,
		Cleanup: func() {
			preparedPrompts.Delete(handoff)
			cleanup()
		},
	}, nil
}

const (
	snapshotHandoffPrefix = "Read the complete task prompt from "
	snapshotHandoffSuffix = " and carry out its instructions. Read the file in full before starting.\n"
)

// SnapshotHandoffPrompt is the entire prompt Prepare uses when the assembled
// text exceeds the size limit.
func SnapshotHandoffPrompt(path string) string {
	return snapshotHandoffPrefix + strconv.Quote(path) + snapshotHandoffSuffix
}

// PreparedPromptText returns the complete text behind a handoff that Prepare
// returned in this process and has not cleaned up. It matches the whole
// prompt only and never opens the file the handoff names.
func PreparedPromptText(handoff string) (string, bool) {
	text, ok := preparedPrompts.Load(handoff)
	if !ok {
		return "", false
	}
	return text.(string), true
}

// LooksLikeSnapshotHandoff reports whether prompt has the shape of a whole
// SnapshotHandoffPrompt, whoever wrote it.
func LooksLikeSnapshotHandoff(prompt string) bool {
	return strings.HasPrefix(prompt, snapshotHandoffPrefix) && strings.HasSuffix(prompt, snapshotHandoffSuffix)
}
