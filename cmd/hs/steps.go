package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/ochanuco/hishaku/internal/agent"
	"github.com/ochanuco/hishaku/internal/config"
	"github.com/ochanuco/hishaku/internal/loan"
)

// hs prepare・push・renew の終了コード。呼ぶ側はこれで分岐する。
// 2 は「他が借りている」に使うので、使い方の誤りも 1 にする。
const (
	exitOK     = 0
	exitFailed = 1
	exitHeld   = 2
	exitStolen = 3
	exitFree   = 5
)

// waitNotice は返してもらうのを待ち始めたことを標準エラーに 1 度出す。
func waitNotice(stderr io.Writer, number int) func(holder string) {
	return func(holder string) {
		fmt.Fprintf(stderr, "hs: %d は %s が使っています。返してもらうのを待っています（Ctrl-C でやめる）\n", number, holder)
	}
}

// interruptible は Ctrl-C と SIGTERM で終わる context を返す。待つ間だけ使う。
func interruptible() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// stepFlags は prepare・push・renew に共通の flag と、番号の読み取りを持つ。
type stepFlags struct {
	fs     *flag.FlagSet
	holder *string
	cwd    *string
}

func newStepFlags(name, usage string, stderr io.Writer, withCwd bool) *stepFlags {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	f := &stepFlags{fs: fs}
	f.holder = fs.String("holder", "", "この機械の名前（既定は HISHAKU_HOLDER、次にホスト名から作る値）")
	if withCwd {
		f.cwd = fs.String("cwd", "", "agent を起こす作業場所（既定はカレント）")
	}
	fs.Usage = func() { fmt.Fprintln(stderr, "使い方: "+usage); fs.PrintDefaults() }
	return f
}

// parse は flag と番号を読む。返す code が -1 でなければ、その値で終える。
func (f *stepFlags) parse(args []string, stderr io.Writer) (int, int) {
	pos, err := parseInterspersed(f.fs, args)
	if errors.Is(err, flag.ErrHelp) {
		return 0, exitOK
	}
	if err != nil {
		return 0, exitFailed
	}
	if len(pos) != 1 {
		f.fs.Usage()
		return 0, exitFailed
	}
	n, err := parseNumber(pos[0])
	if err != nil {
		fmt.Fprintln(stderr, "hs:", err)
		return 0, exitFailed
	}
	return n, -1
}

func (f *stepFlags) config() (*config.Config, error) {
	getenv := os.Getenv
	if *f.holder != "" {
		getenv = func(k string) string {
			if k == "HISHAKU_HOLDER" {
				return *f.holder
			}
			return os.Getenv(k)
		}
	}
	return config.FromEnv(getenv, os.Hostname)
}

func (f *stepFlags) adapter() (loan.AdapterFor, error) {
	cwd, err := resolveDir(*f.cwd)
	if err != nil {
		return nil, err
	}
	return func(name string) (agent.Adapter, error) { return agent.For(name, cwd, os.Getenv) }, nil
}

// stateDir は prepare が push に引き継ぐ状態の置き場所。
func stateDir() (string, error) {
	d, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("状態の置き場所を決められません: %w", err)
	}
	return filepath.Join(d, "hishaku", "prepared"), nil
}

func runPrepare(args []string, stdout, stderr io.Writer) int {
	f := newStepFlags("hs prepare", "hs prepare <number> [--holder <id>] [--cwd <dir>] [--takeover] [--wait] [--json]", stderr, true)
	takeover := f.fs.Bool("takeover", false, "他の holder から貸し出しを奪う")
	wait := f.fs.Bool("wait", false, "他が借りていれば返してほしいと頼み、返されるまで待つ")
	asJSON := f.fs.Bool("json", false, "結果を JSON 1 行で出す")
	number, code := f.parse(args, stderr)
	if code >= 0 {
		return code
	}
	fail := func(err error) int { fmt.Fprintln(stderr, "hs:", err); return exitFailed }
	cfg, err := f.config()
	if err != nil {
		return fail(err)
	}
	adapter, err := f.adapter()
	if err != nil {
		return fail(err)
	}
	dir, err := stateDir()
	if err != nil {
		return fail(err)
	}
	ctx, stop := interruptible()
	defer stop()
	p, err := loan.Prepare(ctx, loan.PrepareOptions{
		Client:   newClient(cfg),
		Number:   number,
		Holder:   cfg.Holder,
		Takeover: *takeover,
		Wait:     *wait,
		OnWait:   waitNotice(stderr, number),
		Adapter:  adapter,
		StateDir: dir,
	})
	if errors.Is(err, context.Canceled) {
		return fail(errors.New("返してもらうのを待つのをやめました"))
	}
	var he *loan.HeldError
	if errors.As(err, &he) {
		if *asJSON {
			_ = json.NewEncoder(stdout).Encode(map[string]any{"number": number, "holder": he.Holder, "lease_expires_at": he.LeaseExpiresAt})
		}
		fmt.Fprintln(stderr, "hs:", he)
		return exitHeld
	}
	if err != nil {
		return fail(err)
	}
	if *asJSON {
		if err := json.NewEncoder(stdout).Encode(p); err != nil {
			return fail(err)
		}
		return exitOK
	}
	fmt.Fprintf(stdout, "number: %d\nagent: %s\nsession_id: %s\nresume: %t\ngeneration: %d\nlease_expires_at: %s\n",
		p.Number, p.Agent, p.SessionID, p.Resume, p.Generation, p.LeaseExpiresAt)
	return exitOK
}

func runPush(args []string, stderr io.Writer) int {
	f := newStepFlags("hs push", "hs push <number> --generation N [--holder <id>] [--cwd <dir>] [--release]", stderr, true)
	generation := f.fs.Int64("generation", 0, "hs prepare が返した世代（必須）")
	release := f.fs.Bool("release", false, "預けた後に貸し出しを返す")
	number, code := f.parse(args, stderr)
	if code >= 0 {
		return code
	}
	fail := func(err error) int { fmt.Fprintln(stderr, "hs:", err); return exitFailed }
	if *generation <= 0 {
		return fail(errors.New("--generation を指定してください"))
	}
	cfg, err := f.config()
	if err != nil {
		return fail(err)
	}
	adapter, err := f.adapter()
	if err != nil {
		return fail(err)
	}
	dir, err := stateDir()
	if err != nil {
		return fail(err)
	}
	err = loan.Push(context.Background(), loan.PushOptions{
		Client:     newClient(cfg),
		Number:     number,
		Holder:     cfg.Holder,
		Generation: *generation,
		Release:    *release,
		Adapter:    adapter,
		StateDir:   dir,
		Log:        stderr,
	})
	if errors.Is(err, loan.ErrStolen) {
		fmt.Fprintln(stderr, "hs:", err)
		return exitStolen
	}
	if err != nil {
		return fail(err)
	}
	return exitOK
}

func runRenew(args []string, stdout, stderr io.Writer) int {
	f := newStepFlags("hs renew", "hs renew <number> --generation N [--holder <id>] [--json]", stderr, false)
	generation := f.fs.Int64("generation", 0, "hs prepare が返した世代（必須）")
	asJSON := f.fs.Bool("json", false, "延ばした後の世代・期限・返してほしいと頼んだ Commander を JSON 1 行で出す")
	number, code := f.parse(args, stderr)
	if code >= 0 {
		return code
	}
	fail := func(err error) int { fmt.Fprintln(stderr, "hs:", err); return exitFailed }
	if *generation <= 0 {
		return fail(errors.New("--generation を指定してください"))
	}
	cfg, err := f.config()
	if err != nil {
		return fail(err)
	}
	r, err := loan.Renew(context.Background(), newClient(cfg), number, cfg.Holder, *generation)
	if errors.Is(err, loan.ErrStolen) {
		fmt.Fprintln(stderr, "hs:", err)
		return exitStolen
	}
	if err != nil {
		return fail(err)
	}
	if *asJSON {
		if err := json.NewEncoder(stdout).Encode(r); err != nil {
			return fail(err)
		}
	}
	return exitOK
}

func runRequest(args []string, stdout, stderr io.Writer) int {
	f := newStepFlags("hs request", "hs request <number> [--holder <id>] [--json]", stderr, false)
	asJSON := f.fs.Bool("json", false, "今の持ち主・世代・期限・頼んだ Commander を JSON 1 行で出す")
	number, code := f.parse(args, stderr)
	if code >= 0 {
		return code
	}
	fail := func(err error) int { fmt.Fprintln(stderr, "hs:", err); return exitFailed }
	cfg, err := f.config()
	if err != nil {
		return fail(err)
	}
	r, err := loan.Request(context.Background(), newClient(cfg), number, cfg.Holder)
	if errors.Is(err, loan.ErrFree) {
		fmt.Fprintf(stderr, "hs: 番号 %d は%s\n", number, err)
		return exitFree
	}
	if err != nil {
		return fail(err)
	}
	if *asJSON {
		if err := json.NewEncoder(stdout).Encode(r); err != nil {
			return fail(err)
		}
		return exitOK
	}
	fmt.Fprintf(stdout, "%s に %d を返してほしいと頼みました\n", r.Holder, number)
	return exitOK
}
