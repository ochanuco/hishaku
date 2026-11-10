// Package turn は hs turn の流れ（借りている番号の agent を非対話で 1 ターン走らせ、記録を預ける）を持つ。
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
	"syscall"
	"time"

	"github.com/ochanuco/hishaku/internal/agent"
	"github.com/ochanuco/hishaku/internal/loan"
	"github.com/ochanuco/hishaku/internal/manager"
)

// Options は hs turn の入力。貸し出しは呼ぶ側が hs prepare で受け、renew で延ばしておく。
type Options struct {
	Client     *manager.Client
	Number     int
	Holder     string
	Generation int64
	Adapter    loan.AdapterFor
	// Dir は agent を起こす作業場所。Adapter の cwd と同じにする。
	Dir string
	// Prompt はこのターンの指示。agent の標準入力に渡す。
	Prompt []byte
	// Extra は agent に渡す追加の引数。
	Extra []string
	// StateDir は hs prepare が残した状態の置き場所。
	StateDir string
	// Log は hs と agent の標準エラーを書く先。
	Log io.Writer
	// Signals は子に渡すシグナルの入口。nil なら SIGINT と SIGTERM を受ける。
	Signals chan os.Signal
}

// Result は hs turn の結果。呼ぶ側は Text を返事として使う。
type Result struct {
	Number    int    `json:"number"`
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

// Run は agent を 1 ターン走らせ、終わったら記録を預ける。貸し出しは返さない。
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

	sid := manager.Str(meta.SessionID)
	resume := false
	if sid != "" {
		_, found, err := a.Locate(sid)
		if err != nil {
			return nil, err
		}
		resume = found
	} else {
		id, err := a.NewSessionID()
		if err != nil {
			return nil, err
		}
		if id != "" {
			if err := l.SetSessionID(ctx, id); err != nil {
				return nil, err
			}
			sid = id
		}
	}

	tmp, err := os.MkdirTemp("", "hs-turn-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	lastMessage := filepath.Join(tmp, "last-message.txt")

	argv := a.TurnCommand(sid, resume, o.Extra, lastMessage)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = o.Dir
	// 席の agent が hs ask と hs status でどの番号の報告かを名乗れるようにする。
	cmd.Env = append(os.Environ(), "HISHAKU_NUMBER="+strconv.Itoa(o.Number))
	var stdout bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = bytes.NewReader(o.Prompt), &stdout, o.Log

	sigs := o.Signals
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

	r := &Result{Number: o.Number, Agent: a.Name(), SessionID: sid}
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
	t, err := a.TurnResult(stdout.Bytes(), lastMessage)
	r.Turn = t
	if err != nil {
		r.Failed = true
		r.Reason = err.Error()
	} else if r.ExitCode != 0 && !r.Failed {
		r.Failed = true
		r.Reason = fmt.Sprintf("%s が終了コード %d で終わりました", a.Name(), r.ExitCode)
	}
	if t.SessionID != "" {
		r.SessionID = t.SessionID
	}

	// 止められた後でも預け終えられるよう、呼ぶ側の ctx とは切り離す。
	pctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if r.SessionID != "" && r.SessionID != sid {
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
