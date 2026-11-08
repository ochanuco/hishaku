package loan

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ochanuco/hishaku/internal/agent"
	"github.com/ochanuco/hishaku/internal/manager"
	"github.com/ochanuco/hishaku/internal/manager/managertest"
)

type env struct {
	home  string
	state string
	fake  *managertest.Fake
	c     *manager.Client
}

func setup(t *testing.T, s *managertest.Session) *env {
	t.Helper()
	e := &env{home: t.TempDir(), state: t.TempDir()}
	e.fake = managertest.New(map[int]*managertest.Session{4821: s})
	t.Cleanup(e.fake.Close)
	e.c = manager.New(e.fake.Server.URL, "", "")
	return e
}

func (e *env) adapter(name string) (agent.Adapter, error) {
	switch name {
	case "claude":
		return &agent.Claude{Home: e.home, Cwd: "/work", Bin: "claude"}, nil
	case "codex":
		return &agent.Codex{Home: e.home, Cwd: "/work", Bin: "codex"}, nil
	}
	return nil, errors.New("unknown")
}

func (e *env) prepare(t *testing.T) *Prepared {
	t.Helper()
	p, err := Prepare(context.Background(), PrepareOptions{Client: e.c, Number: 4821, Holder: "box-a", Adapter: e.adapter, StateDir: e.state})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (e *env) push(gen int64, release bool) error {
	return Push(context.Background(), PushOptions{Client: e.c, Number: 4821, Holder: "box-a", Generation: gen, Release: release, Adapter: e.adapter, StateDir: e.state})
}

func (e *env) claudePath(sid string) string {
	return filepath.Join(e.home, "projects", "-work", sid+".jsonl")
}

func writeTestFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareClaudeExisting(t *testing.T) {
	s := &managertest.Session{Agent: "claude", SessionID: "sid-1", Transcript: []byte("a\n")}
	e := setup(t, s)
	p := e.prepare(t)
	if p.Agent != "claude" || p.SessionID != "sid-1" || !p.Resume || p.Generation != 1 || p.LeaseExpiresAt == "" {
		t.Fatalf("prepared = %+v", p)
	}
	if b, err := os.ReadFile(e.claudePath("sid-1")); err != nil || string(b) != "a\n" {
		t.Fatalf("placed = %q %v", b, err)
	}
	defer e.fake.Lock()()
	if s.Holder != "box-a" {
		t.Fatalf("holder = %q", s.Holder)
	}
}

func TestPrepareClaudeNew(t *testing.T) {
	s := &managertest.Session{Agent: "claude"}
	e := setup(t, s)
	p := e.prepare(t)
	if p.Resume || p.SessionID == "" {
		t.Fatalf("prepared = %+v", p)
	}
	defer e.fake.Lock()()
	if s.SessionID != p.SessionID {
		t.Fatalf("meta session_id = %q, want %q", s.SessionID, p.SessionID)
	}
}

func TestPrepareCodexExisting(t *testing.T) {
	line := `{"timestamp":"2026-01-02T03:04:05.000Z","type":"session_meta","payload":{"id":"codex-sid","cwd":"/work","timestamp":"2026-01-02T03:04:05.000Z"}}` + "\n"
	s := &managertest.Session{Agent: "codex", SessionID: "codex-sid", Transcript: []byte(line)}
	e := setup(t, s)
	p := e.prepare(t)
	if p.Agent != "codex" || p.SessionID != "codex-sid" || !p.Resume {
		t.Fatalf("prepared = %+v", p)
	}
	a, _ := e.adapter("codex")
	if path, found, err := a.Locate("codex-sid"); err != nil || !found {
		t.Fatalf("not placed: %q %v", path, err)
	}
}

func TestPrepareCodexNew(t *testing.T) {
	s := &managertest.Session{Agent: "codex"}
	e := setup(t, s)
	p := e.prepare(t)
	if p.Resume || p.SessionID != "" {
		t.Fatalf("prepared = %+v", p)
	}
	if n := len(e.fake.CallsTo("PATCH", "/4821")); n != 0 {
		t.Fatalf("patched %d times", n)
	}
}

func TestPrepareHeld(t *testing.T) {
	s := &managertest.Session{Agent: "claude", SessionID: "sid-1", Holder: "other-box"}
	e := setup(t, s)
	_, err := Prepare(context.Background(), PrepareOptions{Client: e.c, Number: 4821, Holder: "box-a", Adapter: e.adapter, StateDir: e.state})
	var he *HeldError
	if !errors.As(err, &he) || he.Holder != "other-box" || he.LeaseExpiresAt == "" {
		t.Fatalf("err = %v", err)
	}
}

func TestPushClaude(t *testing.T) {
	s := &managertest.Session{Agent: "claude", SessionID: "sid-1", Transcript: []byte("a\n")}
	e := setup(t, s)
	p := e.prepare(t)

	// 置いたままの記録は PUT しない。
	if err := e.push(p.Generation, false); err != nil {
		t.Fatal(err)
	}
	if n := len(e.fake.CallsTo("PUT", "/transcript")); n != 0 {
		t.Fatalf("puts = %d", n)
	}
	writeTestFile(t, e.claudePath("sid-1"), "a\nb\n")
	if err := e.push(p.Generation, false); err != nil {
		t.Fatal(err)
	}
	if err := e.push(p.Generation, false); err != nil {
		t.Fatal(err)
	}
	if n := len(e.fake.CallsTo("PUT", "/transcript")); n != 1 {
		t.Fatalf("puts = %d", n)
	}
	defer e.fake.Lock()()
	if string(s.Transcript) != "a\nb\n" || s.Holder != "box-a" {
		t.Fatalf("transcript = %q holder = %q", s.Transcript, s.Holder)
	}
}

func TestPushRelease(t *testing.T) {
	s := &managertest.Session{Agent: "claude"}
	e := setup(t, s)
	p := e.prepare(t)
	writeTestFile(t, e.claudePath(p.SessionID), "x\n")
	if err := e.push(p.Generation, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(statePath(e.state, 4821)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state left: %v", err)
	}
	defer e.fake.Lock()()
	if string(s.Transcript) != "x\n" || s.Holder != "" {
		t.Fatalf("transcript = %q holder = %q", s.Transcript, s.Holder)
	}
}

func TestPushEmptyDoesNotPut(t *testing.T) {
	s := &managertest.Session{Agent: "claude"}
	e := setup(t, s)
	p := e.prepare(t)
	writeTestFile(t, e.claudePath(p.SessionID), "")
	if err := e.push(p.Generation, true); err != nil {
		t.Fatal(err)
	}
	if n := len(e.fake.CallsTo("PUT", "/transcript")); n != 0 {
		t.Fatalf("puts = %d", n)
	}
	defer e.fake.Lock()()
	if s.Holder != "" {
		t.Fatalf("holder = %q", s.Holder)
	}
}

func TestPushCodexDiscoversSession(t *testing.T) {
	s := &managertest.Session{Agent: "codex"}
	e := setup(t, s)
	p := e.prepare(t)

	// prepare より前の rollout は今回のものではない。
	now := time.Now()
	dir := filepath.Join(e.home, "sessions", now.Format("2006"), now.Format("01"), now.Format("02"))
	old := filepath.Join(dir, "rollout-2026-01-01T00-00-00-old-sid.jsonl")
	writeTestFile(t, old, `{"type":"session_meta","payload":{"id":"old-sid","cwd":"/work"}}`+"\n")
	if err := os.Chtimes(old, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"session_meta","payload":{"id":"codex-sid","cwd":"/work"}}` + "\nb\n"
	writeTestFile(t, filepath.Join(dir, "rollout-2026-01-01T00-00-01-codex-sid.jsonl"), body)

	if err := e.push(p.Generation, false); err != nil {
		t.Fatal(err)
	}
	defer e.fake.Lock()()
	if s.SessionID != "codex-sid" || string(s.Transcript) != body {
		t.Fatalf("session_id = %q transcript = %q", s.SessionID, s.Transcript)
	}
}

func TestPushCodexBeforeRollout(t *testing.T) {
	s := &managertest.Session{Agent: "codex"}
	e := setup(t, s)
	p := e.prepare(t)
	if err := e.push(p.Generation, false); err != nil {
		t.Fatal(err)
	}
	if n := len(e.fake.CallsTo("PUT", "/transcript")) + len(e.fake.CallsTo("PATCH", "/4821")); n != 0 {
		t.Fatalf("calls = %d", n)
	}
}

func TestPushStolenGoesToForks(t *testing.T) {
	s := &managertest.Session{Agent: "claude", SessionID: "sid-1", Transcript: []byte("a\n")}
	e := setup(t, s)
	p := e.prepare(t)
	func() {
		defer e.fake.Lock()()
		s.Holder, s.Generation = "other-box", s.Generation+1
	}()
	writeTestFile(t, e.claudePath("sid-1"), "a\nb\n")
	err := e.push(p.Generation, true)
	if !errors.Is(err, ErrStolen) {
		t.Fatalf("err = %v", err)
	}
	if n := len(e.fake.CallsTo("DELETE", "/lease")); n != 0 {
		t.Fatalf("released a stolen lease: %d", n)
	}
	defer e.fake.Lock()()
	if string(s.Transcript) != "a\n" || len(s.Forks) != 1 || string(s.Forks[0]) != "a\nb\n" || s.ForkHolders[0] != "box-a" {
		t.Fatalf("transcript = %q forks = %q", s.Transcript, s.Forks)
	}
}

func TestPushWithoutStateNeedsSessionID(t *testing.T) {
	s := &managertest.Session{Agent: "codex"}
	e := setup(t, s)
	err := Push(context.Background(), PushOptions{Client: e.c, Number: 4821, Holder: "box-a", Generation: 1, Adapter: e.adapter})
	if err == nil || !strings.Contains(err.Error(), "hs prepare") {
		t.Fatalf("err = %v", err)
	}
}

func TestRenew(t *testing.T) {
	s := &managertest.Session{Agent: "claude", SessionID: "sid-1"}
	e := setup(t, s)
	p := e.prepare(t)
	r, err := Renew(context.Background(), e.c, 4821, "box-a", p.Generation)
	if err != nil || r.Generation != p.Generation || r.ReleaseRequestedBy != nil {
		t.Fatalf("renew = %+v %v", r, err)
	}
	if _, err := Request(context.Background(), e.c, 4821, "other-box"); err != nil {
		t.Fatal(err)
	}
	r, err = Renew(context.Background(), e.c, 4821, "box-a", p.Generation)
	if err != nil || r.ReleaseRequestedBy == nil || *r.ReleaseRequestedBy != "other-box" {
		t.Fatalf("renew = %+v %v", r, err)
	}
	if _, err := Renew(context.Background(), e.c, 4821, "box-a", p.Generation+1); !errors.Is(err, ErrStolen) {
		t.Fatalf("err = %v", err)
	}
}

func TestRequest(t *testing.T) {
	s := &managertest.Session{Agent: "claude"}
	e := setup(t, s)
	ctx := context.Background()
	if _, err := Request(ctx, e.c, 4821, "other-box"); !errors.Is(err, ErrFree) {
		t.Fatalf("free = %v", err)
	}
	e.prepare(t)
	if _, err := Request(ctx, e.c, 4821, "box-a"); err == nil || errors.Is(err, ErrFree) {
		t.Fatalf("self = %v", err)
	}
	r, err := Request(ctx, e.c, 4821, "other-box")
	if err != nil || r.Holder != "box-a" || r.ReleaseRequestedBy != "other-box" || r.Generation != 1 {
		t.Fatalf("request = %+v %v", r, err)
	}
}

func TestPrepareWaitsForRelease(t *testing.T) {
	s := &managertest.Session{Agent: "claude", SessionID: "sid-1", Holder: "other-box", Generation: 4}
	e := setup(t, s)
	// 頼まれた持ち主が区切りで返す役をする
	go func() {
		for len(e.fake.CallsTo("POST", "/lease/request")) == 0 {
			time.Sleep(5 * time.Millisecond)
		}
		defer e.fake.Lock()()
		s.Holder, s.ReleaseRequestedBy = "", ""
	}()
	var waited []string
	p, err := Prepare(context.Background(), PrepareOptions{
		Client: e.c, Number: 4821, Holder: "box-a", Adapter: e.adapter, StateDir: e.state,
		Wait: true, WaitInterval: 10 * time.Millisecond,
		OnWait: func(holder string) { waited = append(waited, holder) },
	})
	if err != nil || p.Generation != 5 {
		t.Fatalf("prepare = %+v %v", p, err)
	}
	if len(waited) != 1 || waited[0] != "other-box" {
		t.Fatalf("waited = %v", waited)
	}
	if calls := e.fake.CallsTo("POST", "/lease/request"); len(calls) != 1 || calls[0].Body != `{"holder":"box-a"}` {
		t.Fatalf("request calls = %+v", calls)
	}
}

func TestAcquireWaitStopsOnCancel(t *testing.T) {
	s := &managertest.Session{Agent: "claude", Holder: "other-box"}
	e := setup(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := AcquireWait(ctx, e.c, 4821, "box-a", WaitOptions{Interval: 10 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func TestAcquireWaitHeldBySelfDoesNotWait(t *testing.T) {
	s := &managertest.Session{Agent: "claude", Holder: "box-a"}
	e := setup(t, s)
	_, err := AcquireWait(context.Background(), e.c, 4821, "box-a", WaitOptions{})
	var he *HeldError
	if !errors.As(err, &he) || len(e.fake.CallsTo("POST", "/lease/request")) != 0 {
		t.Fatalf("err = %v", err)
	}
}
