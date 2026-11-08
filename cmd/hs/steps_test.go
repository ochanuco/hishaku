package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ochanuco/hishaku/internal/agent"
	"github.com/ochanuco/hishaku/internal/manager/managertest"
)

func setupSteps(t *testing.T, s *managertest.Session) *managertest.Fake {
	t.Helper()
	f := managertest.New(map[int]*managertest.Session{4821: s})
	t.Cleanup(f.Close)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", home+"/cache")
	t.Setenv("CLAUDE_CONFIG_DIR", home+"/claude")
	t.Setenv("HISHAKU_URL", f.Server.URL)
	t.Setenv("HISHAKU_ACCESS_CLIENT_ID", "")
	t.Setenv("HISHAKU_ACCESS_CLIENT_SECRET", "")
	t.Setenv("HISHAKU_HOLDER", "box-a")
	return f
}

func runCmd(args ...string) (int, string, string) {
	var out, errOut strings.Builder
	code := run(args, strings.NewReader(""), &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestPrepareJSON(t *testing.T) {
	setupSteps(t, &managertest.Session{Agent: "claude", SessionID: "sid-1", Transcript: []byte("a\n")})
	code, out, errOut := runCmd("prepare", "4821", "--cwd", t.TempDir(), "--json")
	if code != 0 {
		t.Fatalf("code = %d stderr = %q", code, errOut)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil || strings.Count(out, "\n") != 1 {
		t.Fatalf("out = %q %v", out, err)
	}
	if v["number"] != float64(4821) || v["agent"] != "claude" || v["session_id"] != "sid-1" || v["resume"] != true || v["generation"] != float64(1) || v["lease_expires_at"] == "" {
		t.Fatalf("out = %v", v)
	}
}

func TestPrepareHeldExit2(t *testing.T) {
	setupSteps(t, &managertest.Session{Agent: "claude", SessionID: "sid-1", Holder: "other-box"})
	code, out, _ := runCmd("prepare", "4821", "--json")
	if code != 2 {
		t.Fatalf("code = %d", code)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil || v["holder"] != "other-box" || v["lease_expires_at"] == "" {
		t.Fatalf("out = %q %v", out, err)
	}
}

func TestPrepareHolderFlag(t *testing.T) {
	s := &managertest.Session{Agent: "claude"}
	f := setupSteps(t, s)
	if code, _, errOut := runCmd("prepare", "4821", "--holder", "seat-1", "--json"); code != 0 {
		t.Fatalf("code = %d %q", code, errOut)
	}
	defer f.Lock()()
	if s.Holder != "seat-1" {
		t.Fatalf("holder = %q", s.Holder)
	}
}

func TestPushAndRenewExitCodes(t *testing.T) {
	s := &managertest.Session{Agent: "claude", SessionID: "sid-1", Transcript: []byte("a\n")}
	f := setupSteps(t, s)
	cwd := t.TempDir()
	if code, _, errOut := runCmd("prepare", "4821", "--cwd", cwd, "--json"); code != 0 {
		t.Fatalf("prepare = %d %q", code, errOut)
	}
	gen := strconv.Itoa(1)
	if code, _, errOut := runCmd("renew", "4821", "--generation", gen); code != 0 {
		t.Fatalf("renew = %d %q", code, errOut)
	}
	if code, _, errOut := runCmd("push", "4821", "--generation", gen, "--cwd", cwd); code != 0 {
		t.Fatalf("push = %d %q", code, errOut)
	}
	if code, _, _ := runCmd("push", "4821"); code != 1 {
		t.Fatalf("push without generation = %d", code)
	}
	func() {
		defer f.Lock()()
		s.Holder, s.Generation = "other-box", 2
	}()
	if code, _, _ := runCmd("renew", "4821", "--generation", gen); code != 3 {
		t.Fatalf("stolen renew = %d", code)
	}
	real, err := resolveDir(cwd)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "projects", agent.ProjectDirName(real), "sid-1.jsonl")
	if err := os.WriteFile(p, []byte("a\nb\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := runCmd("push", "4821", "--generation", gen, "--cwd", cwd, "--release"); code != 3 || !strings.Contains(errOut, "forks") {
		t.Fatalf("stolen push = %d %q", code, errOut)
	}
	defer f.Lock()()
	if len(s.Forks) != 1 || string(s.Forks[0]) != "a\nb\n" {
		t.Fatalf("forks = %q", s.Forks)
	}
}

func TestStepsHelp(t *testing.T) {
	for _, c := range []string{"prepare", "push", "renew"} {
		if code, _, errOut := runCmd(c, "--help"); code != 0 || !strings.Contains(errOut, "使い方: hs "+c) {
			t.Fatalf("%s --help = %d %q", c, code, errOut)
		}
	}
}
