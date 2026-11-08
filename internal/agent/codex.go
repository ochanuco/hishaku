package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Codex は Codex CLI のアダプタ。
type Codex struct {
	Home string // CODEX_HOME か ~/.codex
	Cwd  string
	Bin  string
	Now  func() time.Time
}

func (c *Codex) Name() string { return "codex" }

func (c *Codex) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Codex) sessionsDir() string { return filepath.Join(c.Home, "sessions") }

func (c *Codex) Locate(sessionID string) (string, bool, error) {
	suffix := "-" + sessionID + ".jsonl"
	var found string
	err := filepath.WalkDir(c.sessionsDir(), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return filepath.SkipAll
			}
			return err
		}
		if !d.IsDir() && strings.HasPrefix(d.Name(), "rollout-") && strings.HasSuffix(d.Name(), suffix) {
			found = p
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return found, found != "", nil
}

// PlacePath は記録の最初の行の時刻で日付のディレクトリとファイル名を決める。
// 同じ記録をどの機械に置いても同じ日付の下に入り、codex resume の一覧の並びも元の会話の時刻に揃う。
// Codex はファイル名の時刻を手元の時間帯で書くので、ここでも Local に直す。
func (c *Codex) PlacePath(sessionID string, data []byte) (string, error) {
	if p, ok, err := c.Locate(sessionID); err != nil {
		return "", err
	} else if ok {
		return p, nil
	}
	t, ok := firstTimestamp(data)
	if !ok {
		t = c.now()
	}
	t = t.Local()
	name := "rollout-" + t.Format("2006-01-02T15-04-05") + "-" + sessionID + ".jsonl"
	return filepath.Join(c.sessionsDir(), t.Format("2006"), t.Format("01"), t.Format("02"), name), nil
}

// ResumeCommand は追加の引数を resume より前に置く。-a や -s は codex 自体の option なので。
func (c *Codex) ResumeCommand(sessionID string, extra []string) []string {
	argv := append([]string{c.Bin}, extra...)
	return append(argv, "resume", sessionID)
}

// NewCommand は ID を渡さない。Codex は新規のセッション ID を外から指定できない。
func (c *Codex) NewCommand(_ string, extra []string) []string {
	return append([]string{c.Bin}, extra...)
}

func (c *Codex) NewSessionID() (string, error) { return "", nil }

// Discover は since の日から今日までの日付ディレクトリだけを見て、
// cwd が同じで since 以降に書かれた rollout のうち最も新しいものを返す。
func (c *Codex) Discover(since time.Time) (string, string, bool, error) {
	var bestID, bestPath string
	var bestMod time.Time
	start := since.Local()
	day := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.Local)
	for ; !day.After(c.now().Local()); day = day.AddDate(0, 0, 1) {
		dir := filepath.Join(c.sessionsDir(), day.Format("2006"), day.Format("01"), day.Format("02"))
		entries, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", "", false, err
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasPrefix(e.Name(), "rollout-") || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			info, err := e.Info()
			if err != nil || info.ModTime().Before(since) {
				continue
			}
			p := filepath.Join(dir, e.Name())
			id, cwd, ok := readSessionMeta(p)
			if !ok || !sameDir(cwd, c.Cwd) {
				continue
			}
			if bestPath == "" || info.ModTime().After(bestMod) {
				bestID, bestPath, bestMod = id, p, info.ModTime()
			}
		}
	}
	return bestID, bestPath, bestPath != "", nil
}

type codexLine struct {
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Payload   struct {
		ID        string `json:"id"`
		Cwd       string `json:"cwd"`
		Timestamp string `json:"timestamp"`
	} `json:"payload"`
}

func parseFirstLine(data []byte) (codexLine, bool) {
	line, _, _ := bytes.Cut(data, []byte("\n"))
	var l codexLine
	if len(bytes.TrimSpace(line)) == 0 || json.Unmarshal(line, &l) != nil {
		return l, false
	}
	return l, true
}

func firstTimestamp(data []byte) (time.Time, bool) {
	l, ok := parseFirstLine(data)
	if !ok {
		return time.Time{}, false
	}
	for _, s := range []string{l.Payload.Timestamp, l.Timestamp} {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func readSessionMeta(p string) (id, cwd string, ok bool) {
	f, err := os.Open(p)
	if err != nil {
		return "", "", false
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64*1024)
	line, err := r.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return "", "", false
	}
	l, ok := parseFirstLine(line)
	if !ok || l.Type != "session_meta" || l.Payload.ID == "" {
		return "", "", false
	}
	return l.Payload.ID, l.Payload.Cwd, true
}

func sameDir(a, b string) bool {
	if a == b {
		return true
	}
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}
