package resume

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ochanuco/hishaku/internal/agent"
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
	dir := os.Getenv("HS_FAKE_DIR")
	var path string
	if os.Getenv("HS_FAKE_MODE") == "codex" {
		path = filepath.Join(dir, "rollout-2026-01-01T00-00-00-codex-sid.jsonl")
		_ = os.MkdirAll(dir, 0o700)
		_ = os.WriteFile(path, []byte(`{"type":"session_meta","payload":{"id":"codex-sid","cwd":"/work"}}`+"\n"), 0o600)
	} else {
		path = filepath.Join(dir, args[len(args)-1]+".jsonl")
		_ = os.MkdirAll(dir, 0o700)
	}
	before, _ := os.ReadFile(path)
	rec, _ := json.Marshal(map[string]any{"args": args, "before": string(before)})
	_ = os.WriteFile(os.Getenv("HS_FAKE_RECORD"), rec, 0o600)

	appendLine := func(s string) {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		defer f.Close()
		_, _ = f.WriteString(s + "\n")
	}
	pause, _ := strconv.Atoi(os.Getenv("HS_FAKE_PAUSE_MS"))
	appendLine("b")
	time.Sleep(time.Duration(pause) * time.Millisecond)
	appendLine("c")
	code, _ := strconv.Atoi(os.Getenv("HS_FAKE_EXIT"))
	return code
}

type record struct {
	Args   []string `json:"args"`
	Before string   `json:"before"`
}

type env struct {
	home   string
	record string
	opts   Options
	fake   *managertest.Fake
}

func setup(t *testing.T, s *managertest.Session, pause time.Duration) *env {
	t.Helper()
	home := t.TempDir()
	e := &env{home: home, record: filepath.Join(home, "record.json")}
	e.fake = managertest.New(map[int]*managertest.Session{4821: s})
	t.Cleanup(e.fake.Close)

	dir := filepath.Join(home, "projects", "-work")
	if s.Agent == "codex" {
		now := time.Now()
		dir = filepath.Join(home, "sessions", now.Format("2006"), now.Format("01"), now.Format("02"))
		t.Setenv("HS_FAKE_MODE", "codex")
	}
	t.Setenv("HS_FAKE_AGENT", "1")
	t.Setenv("HS_FAKE_DIR", dir)
	t.Setenv("HS_FAKE_RECORD", e.record)
	t.Setenv("HS_FAKE_PAUSE_MS", strconv.Itoa(int(pause/time.Millisecond)))

	e.opts = Options{
		Client: manager.New(e.fake.Server.URL, "", ""),
		Number: 4821,
		Holder: "box-a",
		Adapter: func(name string) (agent.Adapter, error) {
			switch name {
			case "claude":
				return &agent.Claude{Home: home, Cwd: "/work", Bin: os.Args[0]}, nil
			case "codex":
				return &agent.Codex{Home: home, Cwd: "/work", Bin: os.Args[0]}, nil
			}
			return nil, errors.New("unknown")
		},
		Interval: 40 * time.Millisecond,
		TTL:      time.Minute,
		Signals:  make(chan os.Signal, 1),
	}
	return e
}

func (e *env) readRecord(t *testing.T) record {
	t.Helper()
	b, err := os.ReadFile(e.record)
	if err != nil {
		t.Fatal(err)
	}
	var r record
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestResumeExisting(t *testing.T) {
	s := &managertest.Session{Agent: "claude", SessionID: "sid-1", Transcript: []byte("a\n")}
	e := setup(t, s, 300*time.Millisecond)

	code, err := Run(context.Background(), e.opts)
	if err != nil || code != 0 {
		t.Fatalf("run = %d %v", code, err)
	}
	r := e.readRecord(t)
	if strings.Join(r.Args, " ") != "-r sid-1" {
		t.Fatalf("args = %v", r.Args)
	}
	if r.Before != "a\n" {
		t.Fatalf("agent saw %q before start", r.Before)
	}
	defer e.fake.Lock()()
	if string(s.Transcript) != "a\nb\nc\n" {
		t.Fatalf("transcript = %q", s.Transcript)
	}
	if s.Holder != "" {
		t.Fatalf("lease not released: %q", s.Holder)
	}
}

func TestResumePassesExtraArgs(t *testing.T) {
	s := &managertest.Session{Agent: "claude", SessionID: "sid-1", Transcript: []byte("a\n")}
	e := setup(t, s, 0)
	e.opts.Extra = []string{"--permission-mode", "auto"}
	if code, err := Run(context.Background(), e.opts); err != nil || code != 0 {
		t.Fatalf("run = %d %v", code, err)
	}
	if r := e.readRecord(t); strings.Join(r.Args, " ") != "-r sid-1 --permission-mode auto" {
		t.Fatalf("args = %v", r.Args)
	}
}

func TestResumeSyncsWhileRunning(t *testing.T) {
	s := &managertest.Session{Agent: "claude", SessionID: "sid-1", Transcript: []byte("a\n")}
	e := setup(t, s, 300*time.Millisecond)
	if _, err := Run(context.Background(), e.opts); err != nil {
		t.Fatal(err)
	}
	puts := e.fake.CallsTo("PUT", "/transcript")
	if len(puts) != 2 || puts[0].Body != "a\nb\n" || puts[1].Body != "a\nb\nc\n" {
		t.Fatalf("puts = %+v", puts)
	}
	if len(e.fake.CallsTo("POST", "/lease/renew")) == 0 {
		t.Fatal("lease was never renewed")
	}
	if len(e.fake.CallsTo("DELETE", "/lease")) != 1 {
		t.Fatal("lease was not returned")
	}
}

func TestResumeNewClaude(t *testing.T) {
	s := &managertest.Session{Agent: "claude"}
	e := setup(t, s, 0)
	if code, err := Run(context.Background(), e.opts); err != nil || code != 0 {
		t.Fatalf("run = %d %v", code, err)
	}
	r := e.readRecord(t)
	defer e.fake.Lock()()
	if len(r.Args) != 2 || r.Args[0] != "--session-id" || r.Args[1] != s.SessionID || s.SessionID == "" {
		t.Fatalf("args = %v, meta session_id = %q", r.Args, s.SessionID)
	}
	if string(s.Transcript) != "b\nc\n" {
		t.Fatalf("transcript = %q", s.Transcript)
	}
}

func TestResumeNewCodexDiscoversSession(t *testing.T) {
	s := &managertest.Session{Agent: "codex"}
	e := setup(t, s, 200*time.Millisecond)
	if code, err := Run(context.Background(), e.opts); err != nil || code != 0 {
		t.Fatalf("run = %d %v", code, err)
	}
	if r := e.readRecord(t); len(r.Args) != 0 {
		t.Fatalf("args = %v", r.Args)
	}
	defer e.fake.Lock()()
	if s.SessionID != "codex-sid" {
		t.Fatalf("session_id = %q", s.SessionID)
	}
	if !strings.HasSuffix(string(s.Transcript), "b\nc\n") {
		t.Fatalf("transcript = %q", s.Transcript)
	}
}

func TestResumeHeld(t *testing.T) {
	s := &managertest.Session{Agent: "claude", SessionID: "sid-1", Holder: "other-box"}
	e := setup(t, s, 0)
	_, err := Run(context.Background(), e.opts)
	var he *HeldError
	if !errors.As(err, &he) || he.Holder != "other-box" || he.LeaseExpiresAt == "" {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "other-box") {
		t.Fatalf("message = %q", err)
	}
	if _, statErr := os.Stat(e.record); statErr == nil {
		t.Fatal("agent should not start")
	}
}

func TestResumeWaitsForRelease(t *testing.T) {
	s := &managertest.Session{Agent: "claude", SessionID: "sid-1", Holder: "other-box", Transcript: []byte("a\n")}
	e := setup(t, s, 0)
	go func() {
		for len(e.fake.CallsTo("POST", "/lease/request")) == 0 {
			time.Sleep(5 * time.Millisecond)
		}
		defer e.fake.Lock()()
		s.Holder = ""
	}()
	var waited []string
	e.opts.Wait = true
	e.opts.WaitInterval = 10 * time.Millisecond
	e.opts.OnWait = func(holder string) { waited = append(waited, holder) }
	if _, err := Run(context.Background(), e.opts); err != nil {
		t.Fatal(err)
	}
	if len(waited) != 1 || waited[0] != "other-box" {
		t.Fatalf("waited = %v", waited)
	}
	if e.readRecord(t).Before != "a\n" {
		t.Fatal("agent did not start with the transcript")
	}
}

func TestResumeTellsReleaseRequestOnce(t *testing.T) {
	s := &managertest.Session{Agent: "claude", SessionID: "sid-1", Transcript: []byte("a\n")}
	e := setup(t, s, 300*time.Millisecond)
	e.fake.AfterPut = func(s *managertest.Session) { s.ReleaseRequestedBy = "other-box" }
	var log strings.Builder
	e.opts.Log = &log
	if _, err := Run(context.Background(), e.opts); err != nil {
		t.Fatal(err)
	}
	want := "other-box が 4821 を返してほしいと言っています。終わったら agent を閉じてください"
	if strings.Count(log.String(), want) != 1 {
		t.Fatalf("log = %q", log.String())
	}
}

func TestResumeHeldBySelf(t *testing.T) {
	s := &managertest.Session{Agent: "claude", SessionID: "sid-1", Holder: "box-a"}
	e := setup(t, s, 0)
	_, err := Run(context.Background(), e.opts)
	var he *HeldError
	if !errors.As(err, &he) || !strings.Contains(err.Error(), "--takeover で取り直せます") {
		t.Fatalf("err = %v", err)
	}
}

func TestResumeSkipsEmptyTranscript(t *testing.T) {
	s := &managertest.Session{Agent: "claude", SessionID: "sid-1"}
	e := setup(t, s, 0)
	p := filepath.Join(e.home, "projects", "-work", "sid-1.jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	e.opts.Adapter = func(string) (agent.Adapter, error) {
		return &agent.Claude{Home: e.home, Cwd: "/work", Bin: "true"}, nil
	}
	var log strings.Builder
	e.opts.Log = &log
	if code, err := Run(context.Background(), e.opts); err != nil || code != 0 {
		t.Fatalf("run = %d %v", code, err)
	}
	if n := len(e.fake.CallsTo("PUT", "/transcript")); n != 0 || log.Len() != 0 {
		t.Fatalf("puts = %d log = %q", n, log.String())
	}
}

func TestResumeTakeover(t *testing.T) {
	s := &managertest.Session{Agent: "claude", SessionID: "sid-1", Holder: "other-box", Transcript: []byte("a\n")}
	e := setup(t, s, 0)
	e.opts.Takeover = true
	if _, err := Run(context.Background(), e.opts); err != nil {
		t.Fatal(err)
	}
	defer e.fake.Lock()()
	if string(s.Transcript) != "a\nb\nc\n" || s.Holder != "" {
		t.Fatalf("transcript = %q holder = %q", s.Transcript, s.Holder)
	}
}

func TestResumeStolenGoesToForks(t *testing.T) {
	s := &managertest.Session{Agent: "claude", SessionID: "sid-1", Transcript: []byte("a\n")}
	e := setup(t, s, 300*time.Millisecond)
	e.fake.AfterPut = func(s *managertest.Session) {
		s.Holder = "other-box"
		s.Generation++
	}
	var log strings.Builder
	e.opts.Log = &log

	if _, err := Run(context.Background(), e.opts); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "取られました") {
		t.Fatalf("log = %q", log.String())
	}
	if n := len(e.fake.CallsTo("PUT", "/transcript")); n != 1 {
		t.Fatalf("puts after theft: %d", n)
	}
	if n := len(e.fake.CallsTo("DELETE", "/lease")); n != 0 {
		t.Fatalf("released a stolen lease: %d", n)
	}
	defer e.fake.Lock()()
	if string(s.Transcript) != "a\nb\n" {
		t.Fatalf("transcript = %q", s.Transcript)
	}
	if len(s.Forks) != 1 || string(s.Forks[0]) != "a\nb\nc\n" || s.ForkHolders[0] != "box-a" {
		t.Fatalf("forks = %q holders = %q", s.Forks, s.ForkHolders)
	}
}

func TestResumeForwardsSignalThenFinishes(t *testing.T) {
	s := &managertest.Session{Agent: "claude", SessionID: "sid-1", Transcript: []byte("a\n")}
	e := setup(t, s, 10*time.Second)
	go func() {
		time.Sleep(300 * time.Millisecond)
		e.opts.Signals <- syscall.SIGTERM
	}()
	start := time.Now()
	code, err := Run(context.Background(), e.opts)
	if err != nil {
		t.Fatal(err)
	}
	if code == 0 || time.Since(start) > 5*time.Second {
		t.Fatalf("code = %d after %v", code, time.Since(start))
	}
	defer e.fake.Lock()()
	if string(s.Transcript) != "a\nb\n" || s.Holder != "" {
		t.Fatalf("transcript = %q holder = %q", s.Transcript, s.Holder)
	}
}
