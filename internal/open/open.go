// Package open は hs open の流れ（meta を確かめ、Orca に worktree と端末を立てる）を持つ。
package open

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/ochanuco/hishaku/internal/manager"
)

// Runner は外のコマンドを走らせて標準出力を返す。テストでは偽物に差し替える。
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// ExecRunner は本物のコマンドを走らせる。
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// Options は hs open の入力。
type Options struct {
	Client *manager.Client
	Number int
	// Repo は org/repo。空なら meta の repo を使う。
	Repo string
	Yes  bool
	// Orca は orca の実行ファイル。
	Orca   string
	Runner Runner
	In     io.Reader
	Out    io.Writer
}

// ErrCanceled は確かめで断られたときに返る。
var ErrCanceled = errors.New("取りやめました")

// ErrNoRepo は meta にも引数にも repo が無いときに返る。repo を触らずに会話だけを続ける投稿で起きる。
var ErrNoRepo = errors.New("repo が分かりません")

// WorktreeName は番号から worktree の名前を作る。
func WorktreeName(number int) string { return fmt.Sprintf("hs-%d", number) }

// Run は meta を見せて確かめ、worktree が無ければ作り、hs resume を走らせる端末を立てる。
func Run(ctx context.Context, o Options) error {
	meta, err := o.Client.GetSession(ctx, o.Number)
	if err != nil {
		if errors.Is(err, manager.ErrNotFound) {
			return fmt.Errorf("番号 %d はありません", o.Number)
		}
		return fmt.Errorf("番号 %d の meta を読めません: %w", o.Number, err)
	}
	repo := o.Repo
	if repo == "" {
		repo = manager.Str(meta.Repo)
	}
	branch := manager.Str(meta.Branch)

	fmt.Fprintf(o.Out, "番号:     %d\n件名:     %s\nrepo:     %s\nブランチ: %s\nagent:    %s\n",
		meta.Number, meta.Title, orDash(repo), orDash(branch), meta.Agent)
	if repo == "" {
		return ErrNoRepo
	}
	if !o.Yes {
		fmt.Fprint(o.Out, "この内容で開きますか？ [y/N] ")
		line, _ := bufio.NewReader(o.In).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			return ErrCanceled
		}
	}

	r, err := findRepo(ctx, o, repo)
	if err != nil {
		return err
	}
	repoID := r.ID
	name := WorktreeName(o.Number)
	exists, err := hasWorktree(ctx, o, repoID, name)
	if err != nil {
		return err
	}
	if exists {
		fmt.Fprintf(o.Out, "worktree %s はもうあるので、そのまま使います\n", name)
	} else {
		args := []string{"worktree", "create", "--repo", "id:" + repoID, "--name", name}
		if branch != "" {
			// orca の --branch は新しく切るブランチの名前で、起点は --base-branch。
			// 起点を origin の作業ブランチにしないと、席が push した commit の無いブランチが既定の base から切られる。
			if _, err := o.Runner.Run(ctx, "git", "-C", r.Path, "fetch", "origin", branch); err != nil {
				return fmt.Errorf("origin から %s を取れないので worktree を作りません: %w", branch, err)
			}
			args = append(args, "--base-branch", "origin/"+branch, "--branch", branch)
		}
		args = append(args, "--activate", "--json")
		if _, err := orca(ctx, o, args...); err != nil {
			return err
		}
	}
	_, err = orca(ctx, o, "terminal", "create",
		"--worktree", "name:"+name,
		"--command", fmt.Sprintf("hs resume %d", o.Number),
		"--focus", "--json")
	return err
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

func orca(ctx context.Context, o Options, args ...string) (json.RawMessage, error) {
	out, err := o.Runner.Run(ctx, o.Orca, args...)
	if err != nil {
		return nil, err
	}
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil {
		return nil, fmt.Errorf("orca %s: JSON を読めません: %w", strings.Join(args[:2], " "), err)
	}
	if !env.OK {
		return nil, fmt.Errorf("orca %s: %s", strings.Join(args[:2], " "), strings.TrimSpace(string(env.Error)))
	}
	return env.Result, nil
}

type orcaRepo struct {
	ID                string `json:"id"`
	Path              string `json:"path"`
	DisplayName       string `json:"displayName"`
	GitRemoteIdentity *struct {
		CanonicalKey string `json:"canonicalKey"`
	} `json:"gitRemoteIdentity"`
}

// findRepo は org/repo に合う Orca の repo を探す。
// remote の canonicalKey（github.com/org/repo）を先に見て、無ければ path の末尾 org/repo で合わせる。
func findRepo(ctx context.Context, o Options, repo string) (orcaRepo, error) {
	res, err := orca(ctx, o, "repo", "list", "--json")
	if err != nil {
		return orcaRepo{}, err
	}
	var v struct {
		Repos []orcaRepo `json:"repos"`
	}
	if err := json.Unmarshal(res, &v); err != nil {
		return orcaRepo{}, fmt.Errorf("orca repo list: %w", err)
	}
	want := "/" + strings.ToLower(strings.Trim(repo, "/"))
	for _, r := range v.Repos {
		if r.GitRemoteIdentity != nil && strings.HasSuffix("/"+strings.ToLower(r.GitRemoteIdentity.CanonicalKey), want) {
			return r, nil
		}
	}
	for _, r := range v.Repos {
		if strings.HasSuffix(strings.ToLower(filepath.ToSlash(r.Path)), want) {
			return r, nil
		}
	}
	return orcaRepo{}, fmt.Errorf("Orca に %s の repo がありません（orca repo add で足してください）", repo)
}

func hasWorktree(ctx context.Context, o Options, repoID, name string) (bool, error) {
	res, err := orca(ctx, o, "worktree", "list", "--repo", "id:"+repoID, "--json")
	if err != nil {
		return false, err
	}
	var v struct {
		Worktrees []struct {
			RepoID      string `json:"repoId"`
			DisplayName string `json:"displayName"`
		} `json:"worktrees"`
	}
	if err := json.Unmarshal(res, &v); err != nil {
		return false, fmt.Errorf("orca worktree list: %w", err)
	}
	for _, w := range v.Worktrees {
		if w.RepoID == repoID && w.DisplayName == name {
			return true, nil
		}
	}
	return false, nil
}
