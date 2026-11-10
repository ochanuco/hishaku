// Package agent は agent ごとの記録の置き場所と起こし方を持つ。
package agent

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Adapter は 1 種類の agent について、記録の場所と起動のしかたを知っている。
type Adapter interface {
	Name() string
	// Locate は sessionID の記録が手元にあればその場所を返す。
	Locate(sessionID string) (path string, found bool, err error)
	// PlacePath は正本の記録を置く場所を返す。data は記録の中身で、場所が中身に依る agent が使う。
	PlacePath(sessionID string, data []byte) (string, error)
	// ResumeCommand は既存のセッションを再開する argv。extra は agent に渡す追加の引数。
	ResumeCommand(sessionID string, extra []string) []string
	// NewCommand は新規のセッションを始める argv。sessionID は NewSessionID の値。
	NewCommand(sessionID string, extra []string) []string
	// NewSessionID は新規に使う ID を返す。agent が自分で ID を決めるなら空文字。
	NewSessionID() (string, error)
	// Discover は since 以降に新規のセッションが作った記録を探す。NewSessionID が空文字の agent 用。
	Discover(since time.Time) (sessionID, path string, found bool, err error)
	// TurnCommand は 1 ターンを非対話で走らせる argv。指示は標準入力で渡す。
	// lastMessage は最後のメッセージを書かせるファイルで、使わない agent もある。
	TurnCommand(how TurnHow, extra []string, lastMessage string) []string
	// TurnResult は TurnCommand の標準出力と lastMessage からターンの結果を読む。
	TurnResult(stdout []byte, lastMessage string) (Turn, error)
}

// For は agent の名前から Adapter を作る。cwd は agent を起こす作業場所。
func For(name, cwd string, getenv func(string) string) (Adapter, error) {
	switch name {
	case "claude":
		home, err := homeDir(getenv("CLAUDE_CONFIG_DIR"), ".claude")
		if err != nil {
			return nil, err
		}
		return &Claude{Home: home, Cwd: cwd, Bin: "claude"}, nil
	case "codex":
		home, err := homeDir(getenv("CODEX_HOME"), ".codex")
		if err != nil {
			return nil, err
		}
		return &Codex{Home: home, Cwd: cwd, Bin: "codex"}, nil
	}
	return nil, fmt.Errorf("知らない agent です: %q", name)
}

func homeDir(override, dot string) (string, error) {
	if override != "" {
		return override, nil
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, dot), nil
}

// NewUUID は UUID v4 を作る。
func NewUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
