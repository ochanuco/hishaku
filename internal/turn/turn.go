// Package turn は hs turn の流れ（agent を非対話で 1 ターン走らせ、借りている番号なら記録を預ける）を持つ。
package turn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ochanuco/hishaku/internal/agent"
	"github.com/ochanuco/hishaku/internal/loan"
	"github.com/ochanuco/hishaku/internal/manager"
)

// Agent は agent を 1 ターン走らせる入力。Manager を使わない。
type Agent struct {
	Adapter agent.Adapter
	How     agent.TurnHow
	// Number は借りている番号。agent に HISHAKU_NUMBER で渡す。0 なら渡さない。
	Number int
	// Dir は agent を起こす作業場所。Adapter の cwd と同じにする。
	Dir string
	// Prompt はこのターンの指示。agent の標準入力に渡す。
	Prompt []byte
	// Extra は agent に渡す追加の引数。
	Extra []string
	// Log は agent の標準エラーを書く先。
	Log io.Writer
	// Signals は子に渡すシグナルの入口。nil なら SIGINT と SIGTERM を受ける。
	Signals chan os.Signal
}

// Result は hs turn の結果。呼ぶ側は Text を返事として使う。
type Result struct {
	Number    int    `json:"number,omitempty"`
	Agent     string `json:"agent"`
	SessionID string `json:"session_id"`
	// ExitCode は agent の終了コード。
	ExitCode int `json:"exit_code"`
	agent.Turn
	// PushError は記録を預けられなかった理由。ターン自体の成否とは別に知らせる。
	PushError string `json:"push_error,omitempty"`
	// Stolen は貸し出しが他に取られていたこと。記録は forks に預けてある。
	Stolen bool `json:"stolen"`
}

// agentEnv は agent に渡す環境。Manager の service token は agent に要らないので外す。
func agentEnv(number int) []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if k == "HISHAKU_ACCESS_CLIENT_ID" || k == "HISHAKU_ACCESS_CLIENT_SECRET" || k == "HISHAKU_NUMBER" {
			continue
		}
		env = append(env, kv)
	}
	if number != 0 {
		env = append(env, "HISHAKU_NUMBER="+strconv.Itoa(number))
	}
	return env
}

// Exec は agent を 1 ターン走らせて結果を返す。agent を起こす前の失敗だけを error で返す。
func Exec(a Agent) (*Result, error) {
	if a.Log == nil {
		a.Log = io.Discard
	}
	tmp, err := os.MkdirTemp("", "hs-turn-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	lastMessage := filepath.Join(tmp, "last-message.txt")

	argv := a.Adapter.TurnCommand(a.How, a.Extra, lastMessage)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = a.Dir
	cmd.Env = agentEnv(a.Number)
	var stdout bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = bytes.NewReader(a.Prompt), &stdout, a.Log

	sigs := a.Signals
	if sigs == nil {
		sigs = make(chan os.Signal, 4)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		defer signal.Stop(sigs)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%s を起こせません: %w", argv[0], err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var waitErr error
loop:
	for {
		select {
		case waitErr = <-done:
			break loop
		case sig := <-sigs:
			// 止められても記録は預けたいので、hs は子が終わるのを待ってから抜ける。
			_ = cmd.Process.Signal(sig)
		}
	}

	r := &Result{Number: a.Number, Agent: a.Adapter.Name(), SessionID: a.How.SessionID}
	if waitErr != nil {
		var ee *exec.ExitError
		if !errors.As(waitErr, &ee) {
			return nil, waitErr
		}
		r.ExitCode = ee.ExitCode()
		if r.ExitCode <= 0 {
			r.ExitCode = 1
		}
	}
	t, err := a.Adapter.TurnResult(stdout.Bytes(), lastMessage)
	r.Turn = t
	if err != nil {
		r.Failed = true
		r.Reason = err.Error()
	} else if r.ExitCode != 0 && !r.Failed {
		r.Failed = true
		r.Reason = fmt.Sprintf("%s が終了コード %d で終わりました", a.Adapter.Name(), r.ExitCode)
	}
	if t.SessionID != "" {
		r.SessionID = t.SessionID
	}
	return r, nil
}

// Options は借りている番号で走らせるときの入力。貸し出しは呼ぶ側が hs prepare で受け、renew で延ばしておく。
type Options struct {
	Client     *manager.Client
	Number     int
	Holder     string
	Generation int64
	Adapter    loan.AdapterFor
	Dir        string
	Prompt     []byte
	Extra      []string
	// StateDir は hs prepare が残した状態の置き場所。
	StateDir string
	Log      io.Writer
	Signals  chan os.Signal
}

// Run は番号の会話で agent を 1 ターン走らせ、終わったら記録を預ける。貸し出しは返さない。
// agent を起こす前の失敗だけを error で返し、起こした後のことは Result に入れる。
func Run(ctx context.Context, o Options) (*Result, error) {
	if o.Log == nil {
		o.Log = io.Discard
	}
	if o.Adapter == nil {
		return nil, errors.New("adapter が指定されていません")
	}
	meta, err := o.Client.GetSession(ctx, o.Number)
	if err != nil {
		return nil, fmt.Errorf("番号 %d の meta を読めません: %w", o.Number, err)
	}
	a, err := o.Adapter(meta.Agent)
	if err != nil {
		return nil, err
	}
	l := &loan.Loan{Client: o.Client, Number: o.Number, Holder: o.Holder, Generation: o.Generation}

	how := agent.TurnHow{SessionID: manager.Str(meta.SessionID)}
	if how.SessionID != "" {
		_, found, err := a.Locate(how.SessionID)
		if err != nil {
			return nil, err
		}
		how.Resume = found
	} else {
		id, err := a.NewSessionID()
		if err != nil {
			return nil, err
		}
		if id != "" {
			if err := l.SetSessionID(ctx, id); err != nil {
				return nil, err
			}
			how.SessionID = id
		}
	}

	r, err := Exec(Agent{
		Adapter: a, How: how, Number: o.Number, Dir: o.Dir,
		Prompt: o.Prompt, Extra: o.Extra, Log: o.Log, Signals: o.Signals,
	})
	if err != nil {
		return nil, err
	}

	// 止められた後でも預け終えられるよう、呼ぶ側の ctx とは切り離す。
	pctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if r.SessionID != "" && r.SessionID != how.SessionID {
		if err := l.SetSessionID(pctx, r.SessionID); err != nil {
			fmt.Fprintf(o.Log, "hs: %v\n", err)
		}
	}
	err = loan.Push(pctx, loan.PushOptions{
		Client:     o.Client,
		Number:     o.Number,
		Holder:     o.Holder,
		Generation: o.Generation,
		Adapter:    o.Adapter,
		StateDir:   o.StateDir,
		Log:        o.Log,
	})
	switch {
	case errors.Is(err, loan.ErrStolen):
		r.Stolen = true
	case err != nil:
		r.PushError = err.Error()
	}
	return r, nil
}
