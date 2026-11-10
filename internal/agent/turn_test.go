package agent

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestClaudeTurnCommand(t *testing.T) {
	c := &Claude{Bin: "claude"}
	if got := c.TurnCommand("sid", true, []string{"--model", "m"}, ""); !slices.Equal(got, []string{"claude", "-p", "--output-format", "json", "--model", "m", "--resume", "sid"}) {
		t.Fatalf("resume = %v", got)
	}
	if got := c.TurnCommand("sid", false, nil, ""); !slices.Equal(got, []string{"claude", "-p", "--output-format", "json", "--session-id", "sid"}) {
		t.Fatalf("new = %v", got)
	}
}

func TestClaudeTurnResultTakesLastResult(t *testing.T) {
	out := []byte("hook の出力\n" +
		`{"type":"result","subtype":"success","is_error":false,"result":"古い","session_id":"a"}` + "\n" +
		`{"type":"result","subtype":"success","is_error":false,"result":"返事","session_id":"b"}` + "\n")
	got, err := (&Claude{}).TurnResult(out, "")
	if err != nil || got != (Turn{Text: "返事", SessionID: "b"}) {
		t.Fatalf("got = %+v err = %v", got, err)
	}
}

func TestClaudeTurnResultError(t *testing.T) {
	out := []byte(`{"type":"result","subtype":"error_max_turns","is_error":false,"result":"","session_id":"a"}`)
	got, err := (&Claude{}).TurnResult(out, "")
	if err != nil || !got.Failed || got.Reason != "error_max_turns" {
		t.Fatalf("got = %+v err = %v", got, err)
	}
	if _, err := (&Claude{}).TurnResult([]byte("落ちました"), ""); err == nil {
		t.Fatal("result の無い出力を読めました")
	}
}

func TestCodexTurnCommand(t *testing.T) {
	c := &Codex{Bin: "codex"}
	want := []string{"codex", "exec", "-s", "danger-full-access", "--json", "-o", "/tmp/last", "resume", "sid", "-"}
	if got := c.TurnCommand("sid", true, []string{"-s", "danger-full-access"}, "/tmp/last"); !slices.Equal(got, want) {
		t.Fatalf("resume = %v", got)
	}
	if got := c.TurnCommand("", false, nil, "/tmp/last"); !slices.Equal(got, []string{"codex", "exec", "--json", "-o", "/tmp/last", "-"}) {
		t.Fatalf("new = %v", got)
	}
}

func TestCodexTurnResult(t *testing.T) {
	last := filepath.Join(t.TempDir(), "last")
	if err := os.WriteFile(last, []byte("返事\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := []byte(`{"type":"thread.started","thread_id":"t-1"}` + "\n" + `{"type":"item.completed"}` + "\n" + `{"type":"turn.completed"}` + "\n")
	got, err := (&Codex{}).TurnResult(out, last)
	if err != nil || got != (Turn{Text: "返事", SessionID: "t-1"}) {
		t.Fatalf("got = %+v err = %v", got, err)
	}
}

func TestCodexTurnResultFailed(t *testing.T) {
	out := []byte(`{"type":"thread.started","thread_id":"t-1"}` + "\n" + `{"type":"turn.failed","error":{"message":"上限です"}}` + "\n")
	got, err := (&Codex{}).TurnResult(out, filepath.Join(t.TempDir(), "none"))
	if err != nil || !got.Failed || got.Reason != "上限です" || got.Text != "" {
		t.Fatalf("got = %+v err = %v", got, err)
	}
	if _, err := (&Codex{}).TurnResult([]byte(`{"type":"thread.started","thread_id":"t-1"}`), filepath.Join(t.TempDir(), "none")); err == nil {
		t.Fatal("ターンの終わりが無い出力を読めました")
	}
}

func TestCodexTurnResultErrorEvents(t *testing.T) {
	none := filepath.Join(t.TempDir(), "none")
	retried := []byte(`{"type":"error","message":"再接続します"}` + "\n" + `{"type":"turn.completed"}` + "\n")
	if got, err := (&Codex{}).TurnResult(retried, none); err != nil || got.Failed {
		t.Fatalf("再接続の後に終わったターン: got = %+v err = %v", got, err)
	}
	broken := []byte(`{"type":"error","message":"認証が切れました"}` + "\n")
	if got, err := (&Codex{}).TurnResult(broken, none); err != nil || !got.Failed || got.Reason != "認証が切れました" {
		t.Fatalf("終わらなかったターン: got = %+v err = %v", got, err)
	}
}
