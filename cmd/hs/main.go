// hs は agent のセッションを Manager に預け、別の機械で続けるための CLI。
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ochanuco/hishaku/internal/agent"
	"github.com/ochanuco/hishaku/internal/config"
	"github.com/ochanuco/hishaku/internal/manager"
	"github.com/ochanuco/hishaku/internal/open"
	"github.com/ochanuco/hishaku/internal/resume"
)

var version = "dev"

const usage = `hs: agent のセッションを番号で預かり、別の機械で続ける

使い方:
  hs resume [--takeover] <number>      借りて agent を起こし、終わるまで記録を預ける
  hs open [-y] [org/repo] <number>     Orca に worktree hs-<number> と hs resume の端末を立てる
  hs version                           版を出す

環境変数:
  HISHAKU_URL                    Manager の URL（必須）
  HISHAKU_ACCESS_CLIENT_ID       Cloudflare Access の service token の ID
  HISHAKU_ACCESS_CLIENT_SECRET   Cloudflare Access の service token の secret
  HISHAKU_HOLDER                 この機械の名前（既定はホスト名から作る）
  ORCA_CLI_COMMAND               orca の実行ファイル（既定は orca）
  CLAUDE_CONFIG_DIR, CODEX_HOME  agent の記録の置き場所（既定は ~/.claude、~/.codex）
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	case "version", "--version":
		fmt.Fprintln(stdout, version)
		return 0
	case "resume":
		return runResume(args[1:], stderr)
	case "open":
		return runOpen(args[1:], stdin, stdout, stderr)
	}
	fmt.Fprintf(stderr, "hs: 知らないコマンドです: %s\n\n%s", args[0], usage)
	return 2
}

// parseInterspersed は flag を位置引数の後ろに書いても読めるようにする。
// 標準の flag は最初の位置引数で止まるが、`hs resume 1234 --takeover` と打つ人もいるため。
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		if args[0] == "--" {
			return append(pos, args[1:]...), nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func parseNumber(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1000 || n > 9999 {
		return 0, fmt.Errorf("番号は 1000〜9999 の 4 桁です: %q", s)
	}
	return n, nil
}

func newClient(c *config.Config) *manager.Client {
	return manager.New(c.URL, c.ClientID, c.ClientSecret)
}

func runResume(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("hs resume", flag.ContinueOnError)
	fs.SetOutput(stderr)
	takeover := fs.Bool("takeover", false, "他の holder から貸し出しを奪う")
	fs.Usage = func() { fmt.Fprintln(stderr, "使い方: hs resume [--takeover] <number>"); fs.PrintDefaults() }
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) != 1 {
		fs.Usage()
		return 2
	}
	number, err := parseNumber(pos[0])
	if err != nil {
		fmt.Fprintln(stderr, "hs:", err)
		return 2
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(stderr, "hs:", err)
		return 1
	}
	cwd, err := workingDir()
	if err != nil {
		fmt.Fprintln(stderr, "hs:", err)
		return 1
	}
	code, err := resume.Run(context.Background(), resume.Options{
		Client:   newClient(cfg),
		Number:   number,
		Holder:   cfg.Holder,
		Takeover: *takeover,
		Adapter: func(name string) (agent.Adapter, error) {
			return agent.For(name, cwd, os.Getenv)
		},
		Log:              stderr,
		ForwardInterrupt: !isTerminal(os.Stdin),
	})
	if err != nil {
		fmt.Fprintln(stderr, "hs:", err)
	}
	return code
}

func runOpen(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hs open", flag.ContinueOnError)
	fs.SetOutput(stderr)
	yes := fs.Bool("y", false, "確かめずに開く")
	fs.Usage = func() { fmt.Fprintln(stderr, "使い方: hs open [-y] [org/repo] <number>"); fs.PrintDefaults() }
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	var repo string
	switch len(pos) {
	case 1:
	case 2:
		repo = pos[0]
		if strings.Count(repo, "/") != 1 {
			fmt.Fprintf(stderr, "hs: repo は org/repo の形で書いてください: %q\n", repo)
			return 2
		}
		pos = pos[1:]
	default:
		fs.Usage()
		return 2
	}
	number, err := parseNumber(pos[0])
	if err != nil {
		fmt.Fprintln(stderr, "hs:", err)
		return 2
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(stderr, "hs:", err)
		return 1
	}
	orcaBin := os.Getenv("ORCA_CLI_COMMAND")
	if orcaBin == "" {
		orcaBin = "orca"
	}
	err = open.Run(context.Background(), open.Options{
		Client: newClient(cfg),
		Number: number,
		Repo:   repo,
		Yes:    *yes,
		Orca:   orcaBin,
		Runner: open.ExecRunner{},
		In:     stdin,
		Out:    stdout,
	})
	if err != nil {
		fmt.Fprintln(stderr, "hs:", err)
		return 1
	}
	return 0
}

// workingDir は symlink を解いた cwd を返す。
// agent は getcwd の実体の道で記録の場所を決めるので（macOS の /tmp は /private/tmp になる）、それに合わせる。
func workingDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(wd); err == nil {
		return r, nil
	}
	return wd, nil
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
