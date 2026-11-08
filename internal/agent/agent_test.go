package agent

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"
	"time"
)

func TestProjectDirName(t *testing.T) {
	got := ProjectDirName("/Users/someone/src/example.com/my_repo")
	want := "-Users-someone-src-example-com-my-repo"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestForUsesConfigDirs(t *testing.T) {
	env := map[string]string{"CLAUDE_CONFIG_DIR": "/cfg/claude", "CODEX_HOME": "/cfg/codex"}
	getenv := func(k string) string { return env[k] }

	a, err := For("claude", "/work/a.b", getenv)
	if err != nil {
		t.Fatal(err)
	}
	p, err := a.PlacePath("sid-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("/cfg/claude", "projects", "-work-a-b", "sid-1.jsonl"); p != want {
		t.Fatalf("claude path %q, want %q", p, want)
	}
	if got := a.ResumeCommand("sid-1", nil); !reflect.DeepEqual(got, []string{"claude", "-r", "sid-1"}) {
		t.Fatalf("resume %v", got)
	}
	if got := a.NewCommand("sid-1", nil); !reflect.DeepEqual(got, []string{"claude", "--session-id", "sid-1"}) {
		t.Fatalf("new %v", got)
	}

	c, err := For("codex", "/work", getenv)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.(*Codex).Home; got != "/cfg/codex" {
		t.Fatalf("codex home %q", got)
	}
	if got := c.ResumeCommand("sid-2", nil); !reflect.DeepEqual(got, []string{"codex", "resume", "sid-2"}) {
		t.Fatalf("resume %v", got)
	}
	if got := c.NewCommand("", nil); !reflect.DeepEqual(got, []string{"codex"}) {
		t.Fatalf("new %v", got)
	}
	extra := []string{"-a", "never"}
	if got := c.ResumeCommand("sid-2", extra); !reflect.DeepEqual(got, []string{"codex", "-a", "never", "resume", "sid-2"}) {
		t.Fatalf("resume with extra %v", got)
	}
	if got := a.ResumeCommand("sid-1", []string{"--model", "x"}); !reflect.DeepEqual(got, []string{"claude", "-r", "sid-1", "--model", "x"}) {
		t.Fatalf("claude resume with extra %v", got)
	}

	if _, err := For("other", "/work", getenv); err == nil {
		t.Fatal("unknown agent should fail")
	}
}

func TestNewUUID(t *testing.T) {
	id, err := NewUUID()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(id) {
		t.Fatalf("not a v4 uuid: %q", id)
	}
}

func withLocal(t *testing.T, loc *time.Location) {
	old := time.Local
	time.Local = loc
	t.Cleanup(func() { time.Local = old })
}

func TestCodexPlacePathFromFirstLine(t *testing.T) {
	withLocal(t, time.FixedZone("X", 9*3600))
	home := t.TempDir()
	c := &Codex{Home: home, Cwd: "/work", Bin: "codex"}
	data := []byte(`{"timestamp":"2026-01-02T20:30:40.123Z","type":"session_meta","payload":{"id":"sid-3","cwd":"/work"}}` + "\n")
	p, err := c.PlacePath("sid-3", data)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "sessions", "2026", "01", "03", "rollout-2026-01-03T05-30-40-sid-3.jsonl")
	if p != want {
		t.Fatalf("got %q, want %q", p, want)
	}
}

func TestCodexPlacePathFallsBackToNow(t *testing.T) {
	withLocal(t, time.UTC)
	home := t.TempDir()
	now := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	c := &Codex{Home: home, Now: func() time.Time { return now }}
	p, err := c.PlacePath("sid-4", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "sessions", "2026", "03", "04", "rollout-2026-03-04T05-06-07-sid-4.jsonl")
	if p != want {
		t.Fatalf("got %q, want %q", p, want)
	}
}

func TestCodexLocateExisting(t *testing.T) {
	home := t.TempDir()
	existing := filepath.Join(home, "sessions", "2025", "12", "31", "rollout-2025-12-31T23-59-59-sid-5.jsonl")
	if err := os.MkdirAll(filepath.Dir(existing), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(existing, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Codex{Home: home}
	p, ok, err := c.Locate("sid-5")
	if err != nil || !ok || p != existing {
		t.Fatalf("locate = %q %v %v", p, ok, err)
	}
	// 既にあれば、記録の時刻に関わらずその場所に置き直す。
	p, err = c.PlacePath("sid-5", []byte(`{"timestamp":"2026-06-01T00:00:00Z"}`))
	if err != nil || p != existing {
		t.Fatalf("place = %q %v", p, err)
	}
	if _, ok, _ := c.Locate("missing"); ok {
		t.Fatal("missing should not be found")
	}
	if _, ok, err := (&Codex{Home: filepath.Join(home, "none")}).Locate("x"); ok || err != nil {
		t.Fatalf("no sessions dir: %v %v", ok, err)
	}
}

func TestCodexDiscover(t *testing.T) {
	withLocal(t, time.UTC)
	home := t.TempDir()
	now := time.Now().UTC()
	dir := filepath.Join(home, "sessions", now.Format("2006"), now.Format("01"), now.Format("02"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name, id, cwd string) {
		line := `{"timestamp":"2026-01-01T00:00:00Z","type":"session_meta","payload":{"id":"` + id + `","cwd":"` + cwd + `"}}` + "\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(line), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("rollout-a-other.jsonl", "other", "/elsewhere")
	write("rollout-b-mine.jsonl", "mine", "/work")
	c := &Codex{Home: home, Cwd: "/work"}
	id, p, ok, err := c.Discover(now.Add(-time.Minute))
	if err != nil || !ok || id != "mine" || p != filepath.Join(dir, "rollout-b-mine.jsonl") {
		t.Fatalf("discover = %q %q %v %v", id, p, ok, err)
	}
	if _, _, ok, _ := c.Discover(now.Add(time.Hour)); ok {
		t.Fatal("files older than since should be ignored")
	}
}

func TestClaudeLocate(t *testing.T) {
	home := t.TempDir()
	c := &Claude{Home: home, Cwd: "/work"}
	p, ok, err := c.Locate("sid")
	if err != nil || ok {
		t.Fatalf("locate before = %v %v", ok, err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := c.Locate("sid"); err != nil || !ok {
		t.Fatalf("locate after = %v %v", ok, err)
	}
}
