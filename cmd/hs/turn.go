package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/ochanuco/hishaku/internal/report"
	"github.com/ochanuco/hishaku/internal/turn"
)

// exitTurnFailed は agent のターンが失敗で終わったこと。その時も結果の JSON は出す。
const exitTurnFailed = 4

func runTurn(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	f := newStepFlags("hs turn", "hs turn <number> --generation N [--holder <id>] [--cwd <dir>] [--prompt-file <f>] [-- <agent の引数>]", stderr, true)
	generation := f.fs.Int64("generation", 0, "hs prepare が返した世代（必須）")
	promptFile := f.fs.String("prompt-file", "", "このターンの指示のファイル（既定は標準入力）")
	args, extra := splitExtra(args)
	number, code := f.parse(args, stderr)
	if code >= 0 {
		return code
	}
	fail := func(err error) int { fmt.Fprintln(stderr, "hs:", err); return exitFailed }
	if *generation <= 0 {
		return fail(errors.New("--generation を指定してください"))
	}
	var prompt []byte
	var err error
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
	cfg, err := f.config()
	if err != nil {
		return fail(err)
	}
	cwd, err := resolveDir(*f.cwd)
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
	r, err := turn.Run(context.Background(), turn.Options{
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
	if raw == "" {
		return fail(errors.New("番号が分かりません。--number を付けてください"))
	}
	number, err := parseNumber(raw)
	if err != nil {
		return fail(err)
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
	fmt.Fprintf(stderr, "hs: %d の %s を受付に渡しました\n", number, kind)
	return exitOK
}
