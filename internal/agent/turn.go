package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// Turn は非対話で走らせた 1 ターンの結果。
type Turn struct {
	// Text は agent の最後のメッセージ。
	Text string `json:"text"`
	// SessionID は agent が報告したセッション ID。分からなければ空文字。
	SessionID string `json:"session_id"`
	// Failed はターンが失敗で終わったこと。Reason にその理由を入れる。
	Failed bool   `json:"failed"`
	Reason string `json:"reason,omitempty"`
}

// TurnHow は 1 ターンをどの会話で走らせるか。
type TurnHow struct {
	// SessionID は続ける会話か、新規に使う ID。空文字なら agent が決める。
	SessionID string
	// Resume なら SessionID の会話を続ける。
	Resume bool
	// Last なら作業場所の最新の会話を続ける。SessionID より後に見る。
	Last bool
}

func (c *Claude) TurnCommand(how TurnHow, extra []string, _ string) []string {
	argv := append([]string{c.Bin, "-p", "--output-format", "json"}, extra...)
	switch {
	case how.Resume && how.SessionID != "":
		return append(argv, "--resume", how.SessionID)
	case how.Last:
		return append(argv, "--continue")
	case how.SessionID != "":
		return append(argv, "--session-id", how.SessionID)
	}
	return argv
}

type claudeResult struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	IsError   bool   `json:"is_error"`
	Result    string `json:"result"`
	SessionID string `json:"session_id"`
}

// TurnResult は --output-format json が最後に出す result の行を読む。
// hook や設定によっては前に別の行が出ることがあるので、最後の result を採る。
func (c *Claude) TurnResult(stdout []byte, _ string) (Turn, error) {
	var last *claudeResult
	sc := bufio.NewScanner(bytes.NewReader(stdout))
	sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for sc.Scan() {
		var r claudeResult
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.Type == "result" {
			last = &r
		}
	}
	if last == nil {
		return Turn{}, errors.New("claude の出力に result がありません")
	}
	t := Turn{Text: last.Result, SessionID: last.SessionID}
	if last.IsError || last.Subtype != "success" {
		t.Failed = true
		t.Reason = last.Subtype
	}
	return t, nil
}

// TurnCommand は exec の option を resume より前に置く。-s などは exec の option なので。
// 最後のメッセージは --json の出来事から組み立てず、-o で書かせたファイルから読む。
// Codex は新規のセッション ID を外から指定できないので、新規では SessionID を使わない。
func (c *Codex) TurnCommand(how TurnHow, extra []string, lastMessage string) []string {
	argv := append([]string{c.Bin, "exec"}, extra...)
	argv = append(argv, "--json", "-o", lastMessage)
	switch {
	case how.Resume && how.SessionID != "":
		argv = append(argv, "resume", how.SessionID)
	case how.Last:
		argv = append(argv, "resume", "--last")
	}
	return append(argv, "-")
}

type codexEvent struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Message  string `json:"message"`
	Error    struct {
		Message string `json:"message"`
	} `json:"error"`
}

// TurnResult は --json の出来事から thread_id と失敗を拾い、本文は -o のファイルから読む。
func (c *Codex) TurnResult(stdout []byte, lastMessage string) (Turn, error) {
	var t Turn
	sawTurn := false
	lastErr := ""
	sc := bufio.NewScanner(bytes.NewReader(stdout))
	sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for sc.Scan() {
		var e codexEvent
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		switch e.Type {
		case "thread.started":
			t.SessionID = e.ThreadID
		case "turn.completed":
			sawTurn = true
		case "turn.failed":
			sawTurn = true
			t.Failed = true
			t.Reason = e.Error.Message
		case "error":
			// 再接続の途中でも出るので、ターンが終わらなかったときの理由としてだけ使う。
			lastErr = e.Message
		}
	}
	b, err := os.ReadFile(lastMessage)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return t, fmt.Errorf("codex の最後のメッセージを読めません: %w", err)
	}
	t.Text = strings.TrimRight(string(b), "\n")
	if !sawTurn {
		if lastErr != "" {
			t.Failed, t.Reason = true, lastErr
			return t, nil
		}
		return t, errors.New("codex の出力にターンの終わりがありません")
	}
	return t, nil
}
