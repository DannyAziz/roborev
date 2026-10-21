package prompt

import (
	"strconv"
	"strings"

	"go.kenn.io/roborev/internal/config"
)

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
	return SnapshotResult{
		Prompt:   SnapshotHandoffPrompt(file),
		FilePath: file,
		Cleanup:  cleanup,
	}, nil
}

const (
	snapshotHandoffPrefix = "Read the complete task prompt from "
	snapshotHandoffSuffix = " and carry out its instructions. Read the file in full before starting.\n"
)

// SnapshotHandoffPrompt is the entire prompt Prepare uses when the assembled
// text exceeds the size limit. The cloud adapter accepts this exact string
// and no other path reference.
func SnapshotHandoffPrompt(path string) string {
	return snapshotHandoffPrefix + strconv.Quote(path) + snapshotHandoffSuffix
}

// ParseSnapshotHandoffPrompt reports the path when prompt is exactly
// SnapshotHandoffPrompt. A substring, including one copied into a diff, is
// not a handoff.
func ParseSnapshotHandoffPrompt(prompt string) (string, bool) {
	if !strings.HasPrefix(prompt, snapshotHandoffPrefix) || !strings.HasSuffix(prompt, snapshotHandoffSuffix) {
		return "", false
	}
	quoted := strings.TrimSuffix(strings.TrimPrefix(prompt, snapshotHandoffPrefix), snapshotHandoffSuffix)
	path, err := strconv.Unquote(quoted)
	if err != nil || strings.TrimSpace(path) == "" {
		return "", false
	}
	if SnapshotHandoffPrompt(path) != prompt {
		return "", false
	}
	return path, true
}
