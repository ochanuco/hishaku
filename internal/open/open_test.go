package open

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ochanuco/hishaku/internal/manager"
	"github.com/ochanuco/hishaku/internal/manager/managertest"
)

type fakeRunner struct {
	worktrees string
	calls     [][]string
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	switch strings.Join(args[:2], " ") {
	case "repo list":
		return []byte(`{"ok":true,"result":{"repos":[
			{"id":"repo-other","path":"/src/example-org/other","gitRemoteIdentity":{"canonicalKey":"github.com/example-org/other"}},
			{"id":"repo-1","path":"/src/somewhere/app","gitRemoteIdentity":{"canonicalKey":"github.com/Example-Org/App"}},
			{"id":"repo-2","path":"/src/example-org/tool","gitRemoteIdentity":null}
		]}}`), nil
	case "worktree list":
		return []byte(`{"ok":true,"result":{"worktrees":[` + f.worktrees + `]}}`), nil
	case "worktree create", "terminal create":
		return []byte(`{"ok":true,"result":{}}`), nil
	}
	return nil, errors.New("unexpected command")
}

func setup(t *testing.T, s *managertest.Session) (Options, *fakeRunner, *strings.Builder) {
	f := managertest.New(map[int]*managertest.Session{4821: s})
	t.Cleanup(f.Close)
	r := &fakeRunner{}
	out := &strings.Builder{}
	return Options{
		Client: manager.New(f.Server.URL, "", ""),
		Number: 4821,
		Yes:    true,
		Orca:   "/opt/orca",
		Runner: r,
		In:     strings.NewReader(""),
		Out:    out,
	}, r, out
}

func TestOpenCreatesWorktreeAndTerminal(t *testing.T) {
	o, r, out := setup(t, &managertest.Session{Agent: "claude", Title: "件名の例", Repo: "example-org/app", Branch: "dev/example"})
	if err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"/opt/orca", "repo", "list", "--json"},
		{"/opt/orca", "worktree", "list", "--repo", "id:repo-1", "--json"},
		{"/opt/orca", "worktree", "create", "--repo", "id:repo-1", "--name", "hs-4821", "--branch", "dev/example", "--activate", "--json"},
		{"/opt/orca", "terminal", "create", "--worktree", "name:hs-4821", "--command", "hs resume 4821", "--focus", "--json"},
	}
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("calls =\n%q\nwant\n%q", r.calls, want)
	}
	for _, s := range []string{"件名の例", "example-org/app", "dev/example", "claude"} {
		if !strings.Contains(out.String(), s) {
			t.Fatalf("output lacks %q: %s", s, out)
		}
	}
}

func TestOpenReusesWorktree(t *testing.T) {
	o, r, _ := setup(t, &managertest.Session{Agent: "codex", Repo: "example-org/app", Branch: "dev/example"})
	r.worktrees = `{"repoId":"repo-1","displayName":"hs-4821"}`
	if err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	for _, c := range r.calls {
		if c[1] == "worktree" && c[2] == "create" {
			t.Fatalf("created again: %v", c)
		}
	}
	if last := r.calls[len(r.calls)-1]; last[1] != "terminal" {
		t.Fatalf("last call = %v", last)
	}
}

func TestOpenRepoArgOverridesMetaAndMatchesPath(t *testing.T) {
	o, r, _ := setup(t, &managertest.Session{Agent: "claude", Repo: "example-org/app"})
	o.Repo = "example-org/tool"
	if err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	want := []string{"/opt/orca", "worktree", "create", "--repo", "id:repo-2", "--name", "hs-4821", "--activate", "--json"}
	if !reflect.DeepEqual(r.calls[2], want) {
		t.Fatalf("create = %q", r.calls[2])
	}
}

func TestOpenConfirm(t *testing.T) {
	o, r, _ := setup(t, &managertest.Session{Agent: "claude", Repo: "example-org/app"})
	o.Yes = false
	o.In = strings.NewReader("n\n")
	if err := Run(context.Background(), o); !errors.Is(err, ErrCanceled) {
		t.Fatalf("err = %v", err)
	}
	if len(r.calls) != 0 {
		t.Fatalf("ran orca: %v", r.calls)
	}

	o.In = strings.NewReader("y\n")
	if err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
}

func TestOpenErrors(t *testing.T) {
	o, _, _ := setup(t, &managertest.Session{Agent: "claude"})
	if err := Run(context.Background(), o); err == nil || !strings.Contains(err.Error(), "repo") {
		t.Fatalf("no repo: %v", err)
	}
	o.Repo = "example-org/missing"
	if err := Run(context.Background(), o); err == nil || !strings.Contains(err.Error(), "example-org/missing") {
		t.Fatalf("unknown repo: %v", err)
	}
	o.Number = 1111
	if err := Run(context.Background(), o); err == nil || !strings.Contains(err.Error(), "ありません") {
		t.Fatalf("missing number: %v", err)
	}
}
