// Package resume は hs resume の流れ（借りる・置く・起こす・預ける・返す）を持つ。
package resume

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/ochanuco/hishaku/internal/agent"
	"github.com/ochanuco/hishaku/internal/loan"
	"github.com/ochanuco/hishaku/internal/manager"
)

// Options は hs resume の入力。
type Options struct {
	Client   *manager.Client
	Number   int
	Holder   string
	Takeover bool
	// Adapter は meta の agent から Adapter を作る。
	Adapter func(name string) (agent.Adapter, error)
	// Extra は agent に渡す追加の引数。
	Extra []string
	// Interval は記録を見て PUT し、貸し出しを延ばす間隔。
	Interval time.Duration
	// TTL は貸し出しの期限。Interval より十分長くする。
	TTL time.Duration
	// Log は hs 自身の知らせを書く先（agent の出力とは別）。
	Log io.Writer
	// Signals は子に渡すシグナルの入口。nil なら SIGINT と SIGTERM を受ける。
	Signals chan os.Signal
	// ForwardInterrupt が false なら SIGINT を子に送らない。
	ForwardInterrupt bool
}

// HeldError は他の holder が借りていて acquire できなかったことを表す。
type HeldError = loan.HeldError

// Run は貸し出しを受けて agent を起こし、終わるまで記録を預ける。戻り値は agent の終了コード。
func Run(ctx context.Context, o Options) (int, error) {
	if o.Interval <= 0 {
		o.Interval = 30 * time.Second
	}
	if o.Log == nil {
		o.Log = io.Discard
	}
	if o.Adapter == nil {
		return 1, errors.New("adapter が指定されていません")
	}

	l, err := loan.Acquire(ctx, o.Client, o.Number, o.Holder, o.Takeover, o.TTL)
	if err != nil {
		var he *HeldError
		// Manager は期限内なら自分の貸し出しでも acquire を断る。前の hs が落ちて返さなかった場合にこうなる。
		if errors.As(err, &he) && he.Holder == o.Holder {
			return 1, fmt.Errorf("%w。持ち主はこの機械なので、前の hs が残っていなければ --takeover で取り直せます", he)
		}
		return 1, err
	}

	s := &session{o: o, loan: l}
	p, err := l.Place(ctx, o.Adapter)
	if err != nil {
		s.release()
		return 1, err
	}
	s.adapter, s.sessionID, s.path, s.lastSum = p.Agent, p.Session, p.Path, p.Sum
	// agent が ID を自分で決めるので、起こした後に Discover で見つけて meta に残す。
	s.needPatch = p.Path == ""
	argv := p.Command(o.Extra)

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	sigs := o.Signals
	if sigs == nil {
		sigs = make(chan os.Signal, 4)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		defer signal.Stop(sigs)
	}
	s.started = time.Now()
	if err := cmd.Start(); err != nil {
		s.release()
		return 1, fmt.Errorf("%s を起こせません: %w", argv[0], err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	tick := time.NewTicker(o.Interval)
	defer tick.Stop()

	var waitErr error
loop:
	for {
		select {
		case waitErr = <-done:
			break loop
		case sig := <-sigs:
			// 端末の Ctrl-C は前面のプロセスグループ全体に届くので、子はもう受け取っている。
			// ここで重ねて送ると Claude Code は 2 回押されたと見て終わってしまう。
			if sig == syscall.SIGINT && !o.ForwardInterrupt {
				continue
			}
			_ = cmd.Process.Signal(sig)
		case <-tick.C:
			s.sync(ctx, false)
		}
	}

	s.sync(context.Background(), true)
	s.release()

	if waitErr != nil {
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			if code := ee.ExitCode(); code > 0 {
				return code, nil
			}
			return 1, nil
		}
		return 1, waitErr
	}
	return 0, nil
}

type session struct {
	o         Options
	loan      *loan.Loan
	adapter   agent.Adapter
	sessionID string
	path      string
	started   time.Time
	// needPatch は session_id を meta にまだ残せていないこと。
	needPatch bool
	lastSum   [32]byte
	stolen    bool
	forkSum   [32]byte
}

func (s *session) logf(format string, args ...any) {
	fmt.Fprintf(s.o.Log, "hs: "+format+"\n", args...)
}

// sync は記録が変わっていれば預け、final でなければ貸し出しを延ばす。
// 失敗は知らせるだけで続ける。次の回か最後の回で取り返せるため。
func (s *session) sync(ctx context.Context, final bool) {
	if s.path == "" {
		id, p, found, err := s.adapter.Discover(s.started)
		if err != nil {
			s.logf("記録を探せません: %v", err)
		}
		if !found {
			s.renew(ctx, final)
			return
		}
		s.sessionID, s.path = id, p
	}
	if s.needPatch {
		if err := s.loan.SetSessionID(ctx, s.sessionID); err != nil {
			s.logf("%v", err)
		} else {
			s.needPatch = false
		}
	}

	data, err := os.ReadFile(s.path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.logf("記録を読めません: %v", err)
		}
		s.renew(ctx, final)
		return
	}
	// Manager は長さ 0 の本文を 411 で断るうえ、空の記録は預ける中身も無い。
	if len(data) == 0 {
		s.renew(ctx, final)
		return
	}
	sum := sha256.Sum256(data)

	if !s.stolen && sum != s.lastSum {
		err := s.loan.Put(ctx, data)
		switch {
		case err == nil:
			s.lastSum = sum
		case errors.Is(err, loan.ErrStolen):
			s.markStolen()
		default:
			s.logf("記録を預けられません: %v", err)
		}
	}
	if !s.stolen {
		s.renew(ctx, final)
	}
	// 取られた後は 30 秒ごとに枝を増やさず、取られた時と最後にだけ預ける。
	// forks は呼ぶたびに別の枝になるので、毎回送ると同じ会話の枝が何十本もできる。
	if s.stolen && sum != s.forkSum && (final || s.forkSum == s.lastSum) {
		key, err := s.loan.Fork(ctx, data)
		if err != nil {
			s.logf("記録を forks に預けられません: %v", err)
			return
		}
		s.forkSum = sum
		s.logf("記録を forks に預けました: %s", key)
	}
}

func (s *session) renew(ctx context.Context, final bool) {
	if final || s.stolen {
		return
	}
	err := s.loan.Renew(ctx)
	switch {
	case err == nil:
	case errors.Is(err, loan.ErrStolen):
		s.markStolen()
	default:
		s.logf("貸し出しを延ばせません: %v", err)
	}
}

func (s *session) markStolen() {
	if s.stolen {
		return
	}
	s.stolen = true
	s.forkSum = s.lastSum
	s.logf("番号 %d の貸し出しは他に取られました。以後の記録は forks に預けます", s.o.Number)
}

func (s *session) release() {
	if s.stolen {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := s.loan.Release(ctx)
	switch {
	case err == nil:
	case errors.Is(err, loan.ErrStolen):
		s.logf("番号 %d の貸し出しは他に取られていました", s.o.Number)
	default:
		s.logf("貸し出しを返せません: %v", err)
	}
}
