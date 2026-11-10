package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/ochanuco/hishaku/internal/agent"
	"github.com/ochanuco/hishaku/internal/report"
	"github.com/ochanuco/hishaku/internal/turn"
)

// exitTurnFailed は agent のターンが失敗で終わったこと。その時も結果の JSON は出す。
const exitTurnFailed = 4

func runTurn(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	f := newStepFlags("hs turn", "hs turn <number> --generation N | --agent <claude|codex> [--session <id> [--resume] | --continue]  [--holder <id>] [--cwd <dir>] [--prompt-file <f>] [-- <agent の引数>]", stderr, true)
	generation := f.fs.Int64("generation", 0, "hs prepare が返した世代（番号で走らせるときは必須）")
	promptFile := f.fs.String("prompt-file", "", "このターンの指示のファイル（既定は標準入力）")
	agentName := f.fs.String("agent", "", "番号なしで走らせる agent")
	session := f.fs.String("session", "", "番号なしのとき、続けるか新規に使うセッション ID")
	resumeFlag := f.fs.Bool("resume", false, "番号なしのとき、--session の会話を続ける")
	cont := f.fs.Bool("continue", false, "番号なしのとき、作業場所の最新の会話を続ける")
	args, extra := splitExtra(args)
	pos, err := parseInterspersed(f.fs, args)
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	if err != nil {
		return exitFailed
	}
	fail := func(err error) int { fmt.Fprintln(stderr, "hs:", err); return exitFailed }
	local := *agentName != ""
	switch {
	case local && len(pos) != 0, !local && len(pos) != 1:
		f.fs.Usage()
		return exitFailed
	case !local && (*session != "" || *resumeFlag || *cont):
		return fail(errors.New("--session、--resume、--continue は --agent と使います。番号の会話は meta で決まります"))
	case *resumeFlag && *session == "":
		return fail(errors.New("--resume には --session が要ります"))
	}
	var prompt []byte
	if *promptFile != "" {
		prompt, err = os.ReadFile(*promptFile)
	} else {
		prompt, err = io.ReadAll(stdin)
	}
	if err != nil {
		return fail(fmt.Errorf("指示を読めません: %w", err))
	}
	if len(prompt) == 0 {
		return fail(errors.New("指示が空です"))
	}
	cwd, err := resolveDir(*f.cwd)
	if err != nil {
		return fail(err)
	}

	var r *turn.Result
	if local {
		a, err := agent.For(*agentName, cwd, os.Getenv)
		if err != nil {
			return fail(err)
		}
		r, err = turn.Exec(turn.Agent{
			Adapter: a,
			How:     agent.TurnHow{SessionID: *session, Resume: *resumeFlag, Last: *cont},
			Dir:     cwd, Prompt: prompt, Extra: extra, Log: stderr,
		})
		if err != nil {
			return fail(err)
		}
	} else {
		number, err := parseNumber(pos[0])
		if err != nil {
			return fail(err)
		}
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
		r, err = turn.Run(context.Background(), turn.Options{
			Client:     newClient(cfg),
			Number:     number,
			Holder:     cfg.Holder,
			Generation: *generation,
			Adapter:    adapter,
			Dir:        cwd,
			Prompt:     prompt,
			Extra:      extra,
			StateDir:   dir,
			Log:        stderr,
		})
		if err != nil {
			return fail(err)
		}
	}
	if err := json.NewEncoder(stdout).Encode(r); err != nil {
		return fail(err)
	}
	switch {
	case r.Stolen:
		return exitStolen
	case r.Failed:
		return exitTurnFailed
	}
	return exitOK
}

func runReport(kind report.Kind, args []string, stdin io.Reader, stderr io.Writer) int {
	name := "hs " + string(kind)
	f := newStepFlags(name, name+" [--subject <件名>] --body <本文|->", stderr, false)
	subject := f.fs.String("subject", "", "件名")
	body := f.fs.String("body", "", "本文。- なら標準入力から読む")
	numberFlag := f.fs.String("number", "", "番号（既定は hs turn が渡す HISHAKU_NUMBER）")
	pos, err := parseInterspersed(f.fs, args)
	if err != nil {
		return exitFailed
	}
	fail := func(err error) int { fmt.Fprintln(stderr, "hs:", err); return exitFailed }
	if len(pos) != 0 {
		f.fs.Usage()
		return exitFailed
	}
	raw := *numberFlag
	if raw == "" {
		raw = os.Getenv("HISHAKU_NUMBER")
	}
	// 番号の無い席もある。送り先はそれを URL で見分ける。
	number := 0
	if raw != "" {
		if number, err = parseNumber(raw); err != nil {
			return fail(err)
		}
	}
	text := *body
	if text == "-" {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return fail(fmt.Errorf("本文を読めません: %w", err))
		}
		text = string(b)
	}
	t, err := report.FromEnv(os.Getenv)
	if err != nil {
		return fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := t.Send(ctx, report.Report{Number: number, Type: kind, Subject: *subject, Body: text}); err != nil {
		return fail(err)
	}
	fmt.Fprintf(stderr, "hs: %s を受付に渡しました\n", kind)
	return exitOK
}
