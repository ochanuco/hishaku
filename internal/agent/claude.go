package agent

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Claude は Claude Code のアダプタ。
type Claude struct {
	Home string // CLAUDE_CONFIG_DIR か ~/.claude
	Cwd  string
	Bin  string
}

func (c *Claude) Name() string { return "claude" }

// ProjectDirName は cwd から projects の下のディレクトリ名を作る。
// Claude Code は / と . だけでなく英数字以外をすべて - にする（_ も - になる）ので、それに合わせる。
func ProjectDirName(cwd string) string {
	var b strings.Builder
	for _, r := range cwd {
		if r < 0x80 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	return b.String()
}

func (c *Claude) path(sessionID string) string {
	return filepath.Join(c.Home, "projects", ProjectDirName(c.Cwd), sessionID+".jsonl")
}

func (c *Claude) Locate(sessionID string) (string, bool, error) {
	p := c.path(sessionID)
	_, err := os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return p, false, nil
	}
	if err != nil {
		return "", false, err
	}
	return p, true, nil
}

func (c *Claude) PlacePath(sessionID string, _ []byte) (string, error) {
	return c.path(sessionID), nil
}

func (c *Claude) ResumeCommand(sessionID string, extra []string) []string {
	return append([]string{c.Bin, "-r", sessionID}, extra...)
}

func (c *Claude) NewCommand(sessionID string, extra []string) []string {
	return append([]string{c.Bin, "--session-id", sessionID}, extra...)
}

func (c *Claude) NewSessionID() (string, error) { return NewUUID() }

func (c *Claude) Discover(time.Time) (string, string, bool, error) { return "", "", false, nil }
