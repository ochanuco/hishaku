package turn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/ochanuco/hishaku/internal/agent"
	"github.com/ochanuco/hishaku/internal/loan"
	"github.com/ochanuco/hishaku/internal/manager"
	"github.com/ochanuco/hishaku/internal/manager/managertest"
)

// TestMain は HS_FAKE_AGENT が立っていれば、このテストバイナリ自身を偽の agent として動かす。
func TestMain(m *testing.M) {
	if os.Getenv("HS_FAKE_AGENT") == "1" {
		os.Exit(fakeAgent())
	}
	os.Exit(m.Run())
}

func fakeAgent() int {
	args := os.Args[1:]
	prompt, _ := io.ReadAll(os.Stdin)
	cwd, _ := os.Getwd()
	rec, _ := json.Marshal(record{Args: args, Prompt: string(prompt), Number: os.Getenv("HISHAKU_NUMBER"), Cwd: cwd})
	_ = os.WriteFile(os.Getenv("HS_FAKE_RECORD"), rec, 0o600)

	transcript := os.Getenv("HS_FAKE_TRANSCRIPT")
	_ = os.MkdirAll(filepath.Dir(transcript), 0o700)
	f, _ := os.OpenFile(transcript, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	_, _ = f.WriteString(os.Getenv("HS_FAKE_LINE") + "\n")
	_ = f.Close()

	failed := os.Getenv("HS_FAKE_FAIL") == "1"
	if os.Getenv("HS_FAKE_MODE") == "codex" {
		out := args[slices.Index(args, "-o")+1]
		_ = os.WriteFile(out, []byte("codex の返事\n"), 0o600)
		fmt.Println(`{"type":"thread.started","thread_id":"codex-sid"}`)
		if failed {
			fmt.Println(`{"type":"turn.failed","error":{"message":"止まりました"}}`)
			return 1
		}
		fmt.Println(`{"type":"turn.completed"}`)
		return 0
	}
	sid := args[len(args)-1]
	if failed {
		fmt.Printf(`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"","session_id":%q}`+"\n", sid)
		return 1
	}
	fmt.Printf(`{"type":"result","subtype":"success","is_error":false,"result":"claude の返事","session_id":%q}`+"\n", sid)
	return 0
}

type record struct {
	Args   []string `json:"args"`
	Prompt string   `json:"prompt"`
	Number string   `json:"number"`
	Cwd    string   `json:"cwd"`
}

type env struct {
	home  string
	cwd   string
	state string
	rec   string
	fake  *managertest.Fake
	c     *manager.Client
	s     *managertest.Session
}

func setup(t *testing.T, s *managertest.Session) *env {
	t.Helper()
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &env{home: t.TempDir(), cwd: cwd, state: t.TempDir(), s: s}
	e.rec = filepath.Join(e.home, "record.json")
	e.fake = managertest.New(map[int]*managertest.Session{4821: s})
	t.Cleanup(e.fake.Close)
	e.c = manager.New(e.fake.Server.URL, "", "")
	t.Setenv("HS_FAKE_AGENT", "1")
	t.Setenv("HS_FAKE_RECORD", e.rec)
	t.Setenv("HS_FAKE_LINE", "b")
	if s.Agent == "codex" {
		t.Setenv("HS_FAKE_MODE", "codex")
		now := time.Now()
		dir := filepath.Join(e.home, "sessions", now.Format("2006"), now.Format("01"), now.Format("02"))
		p := filepath.Join(dir, "rollout-2026-01-01T00-00-00-codex-sid.jsonl")
		t.Setenv("HS_FAKE_TRANSCRIPT", p)
		t.Setenv("HS_FAKE_LINE", fmt.Sprintf(`{"type":"session_meta","payload":{"id":"codex-sid","cwd":%q}}`, cwd))
	}
	return e
}

func (e *env) adapter(name string) (agent.Adapter, error) {
	switch name {
	case "claude":
		return &agent.Claude{Home: e.home, Cwd: e.cwd, Bin: os.Args[0]}, nil
	case "codex":
		return &agent.Codex{Home: e.home, Cwd: e.cwd, Bin: os.Args[0]}, nil
	}
	return nil, errors.New("unknown")
}

func (e *env) claudePath(sid string) string {
	return filepath.Join(e.home, "projects", agent.ProjectDirName(e.cwd), sid+".jsonl")
}

func (e *env) prepare(t *testing.T) *loan.Prepared {
	t.Helper()
	p, err := loan.Prepare(context.Background(), loan.PrepareOptions{Client: e.c, Number: 4821, Holder: "box-a", Adapter: e.adapter, StateDir: e.state})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (e *env) run(t *testing.T, gen int64) *Result {
	t.Helper()
	r, err := Run(context.Background(), Options{
		Client: e.c, Number: 4821, Holder: "box-a", Generation: gen, Adapter: e.adapter,
		Dir: e.cwd, Prompt: []byte("続きをどうぞ"), Extra: []string{"--model", "m"}, StateDir: e.state,
		Signals: make(chan os.Signal),
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (e *env) record(t *testing.T) record {
	t.Helper()
	b, err := os.ReadFile(e.rec)
	if err != nil {
		t.Fatal(err)
	}
	var r record
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestClaudeNewSession(t *testing.T) {
	e := setup(t, &managertest.Session{Agent: "claude"})
	p := e.prepare(t)
	t.Setenv("HS_FAKE_TRANSCRIPT", e.claudePath(p.SessionID))

	r := e.run(t, p.Generation)
	if r.Failed || r.Text != "claude の返事" || r.SessionID != p.SessionID || r.PushError != "" {
		t.Fatalf("result = %+v", r)
	}
	rec := e.record(t)
	want := []string{"-p", "--output-format", "json", "--model", "m", "--session-id", p.SessionID}
	if !slices.Equal(rec.Args, want) || rec.Prompt != "続きをどうぞ" || rec.Number != "4821" || rec.Cwd != e.cwd {
		t.Fatalf("record = %+v", rec)
	}
	defer e.fake.Lock()()
	if string(e.s.Transcript) != "b\n" || e.s.Holder != "box-a" {
		t.Fatalf("transcript = %q holder = %q", e.s.Transcript, e.s.Holder)
	}
}

func TestClaudeResume(t *testing.T) {
	e := setup(t, &managertest.Session{Agent: "claude", SessionID: "sid-1", Transcript: []byte("a\n")})
	p := e.prepare(t)
	t.Setenv("HS_FAKE_TRANSCRIPT", e.claudePath("sid-1"))

	r := e.run(t, p.Generation)
	if r.Failed {
		t.Fatalf("result = %+v", r)
	}
	if args := e.record(t).Args; !slices.Equal(args[len(args)-2:], []string{"--resume", "sid-1"}) {
		t.Fatalf("args = %v", args)
	}
	defer e.fake.Lock()()
	if string(e.s.Transcript) != "a\nb\n" {
		t.Fatalf("transcript = %q", e.s.Transcript)
	}
}

func TestClaudeFailedTurnStillPushes(t *testing.T) {
	e := setup(t, &managertest.Session{Agent: "claude", SessionID: "sid-1", Transcript: []byte("a\n")})
	p := e.prepare(t)
	t.Setenv("HS_FAKE_TRANSCRIPT", e.claudePath("sid-1"))
	t.Setenv("HS_FAKE_FAIL", "1")

	r := e.run(t, p.Generation)
	if !r.Failed || r.Reason != "error_during_execution" || r.ExitCode != 1 {
		t.Fatalf("result = %+v", r)
	}
	defer e.fake.Lock()()
	if string(e.s.Transcript) != "a\nb\n" {
		t.Fatalf("transcript = %q", e.s.Transcript)
	}
}

func TestCodexNewSession(t *testing.T) {
	e := setup(t, &managertest.Session{Agent: "codex"})
	p := e.prepare(t)

	r := e.run(t, p.Generation)
	if r.Failed || r.Text != "codex の返事" || r.SessionID != "codex-sid" || r.PushError != "" {
		t.Fatalf("result = %+v", r)
	}
	args := e.record(t).Args
	if args[0] != "exec" || args[len(args)-1] != "-" || slices.Contains(args, "resume") {
		t.Fatalf("args = %v", args)
	}
	defer e.fake.Lock()()
	if e.s.SessionID != "codex-sid" || len(e.s.Transcript) == 0 {
		t.Fatalf("session_id = %q transcript = %q", e.s.SessionID, e.s.Transcript)
	}
}

func TestCodexFailedTurn(t *testing.T) {
	e := setup(t, &managertest.Session{Agent: "codex"})
	p := e.prepare(t)
	t.Setenv("HS_FAKE_FAIL", "1")

	r := e.run(t, p.Generation)
	if !r.Failed || r.Reason != "止まりました" || r.ExitCode != 1 {
		t.Fatalf("result = %+v", r)
	}
}

func TestStolenGoesToForks(t *testing.T) {
	e := setup(t, &managertest.Session{Agent: "claude", SessionID: "sid-1", Transcript: []byte("a\n")})
	p := e.prepare(t)
	t.Setenv("HS_FAKE_TRANSCRIPT", e.claudePath("sid-1"))
	func() {
		defer e.fake.Lock()()
		e.s.Holder = "mac-b"
		e.s.Generation++
	}()

	r := e.run(t, p.Generation)
	if !r.Stolen || r.Text != "claude の返事" {
		t.Fatalf("result = %+v", r)
	}
	defer e.fake.Lock()()
	if len(e.s.Forks) != 1 || string(e.s.Transcript) != "a\n" {
		t.Fatalf("forks = %d transcript = %q", len(e.s.Forks), e.s.Transcript)
	}
}
