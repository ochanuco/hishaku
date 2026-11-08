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
	"path/filepath"
	"syscall"
	"time"

	"github.com/ochanuco/hishaku/internal/agent"
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
type HeldError struct {
	Number         int
	Holder         string
	LeaseExpiresAt string
}

func (e *HeldError) Error() string {
	return fmt.Sprintf("番号 %d は %s が %s まで借りています（--takeover で奪えます）", e.Number, e.Holder, e.LeaseExpiresAt)
}

// Run は貸し出しを受けて agent を起こし、終わるまで記録を預ける。戻り値は agent の終了コード。
func Run(ctx context.Context, o Options) (int, error) {
	if o.Interval <= 0 {
		o.Interval = 30 * time.Second
	}
	if o.TTL <= 0 {
		o.TTL = 2 * time.Minute
	}
	if o.Log == nil {
		o.Log = io.Discard
	}
	if o.Adapter == nil {
		return 1, errors.New("adapter が指定されていません")
	}

	mode := manager.Acquire
	if o.Takeover {
		mode = manager.Takeover
	}
	lease, err := o.Client.AcquireLease(ctx, o.Number, o.Holder, mode, o.TTL)
	if err != nil {
		var ce *manager.ConflictError
		if errors.As(err, &ce) {
			return 1, &HeldError{Number: o.Number, Holder: ce.Holder, LeaseExpiresAt: ce.LeaseExpiresAt}
		}
		return 1, fmt.Errorf("貸し出しを受けられません: %w", err)
	}

	s := &session{o: o, generation: lease.Generation}
	argv, err := s.prepare(ctx)
	if err != nil {
		s.release()
		return 1, err
	}

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
	o          Options
	adapter    agent.Adapter
	generation int64
	sessionID  string
	path       string
	started    time.Time
	// needPatch は session_id を meta にまだ残せていないこと。
	needPatch bool
	lastSum   [32]byte
	stolen    bool
	forkSum   [32]byte
}

// prepare は記録を置き、起こす argv を返す。
func (s *session) prepare(ctx context.Context) ([]string, error) {
	meta, err := s.o.Client.GetSession(ctx, s.o.Number)
	if err != nil {
		return nil, fmt.Errorf("番号 %d の meta を読めません: %w", s.o.Number, err)
	}
	s.adapter, err = s.o.Adapter(meta.Agent)
	if err != nil {
		return nil, err
	}
	sid := manager.Str(meta.SessionID)

	data, _, err := s.o.Client.GetTranscript(ctx, s.o.Number)
	switch {
	case err == nil:
		if sid == "" {
			return nil, fmt.Errorf("番号 %d は記録があるのに session_id がありません", s.o.Number)
		}
		p, err := s.adapter.PlacePath(sid, data)
		if err != nil {
			return nil, err
		}
		if err := writeFile(p, data); err != nil {
			return nil, fmt.Errorf("記録を置けません: %w", err)
		}
		s.sessionID, s.path, s.lastSum = sid, p, sha256.Sum256(data)
		return s.adapter.ResumeCommand(sid), nil
	case !errors.Is(err, manager.ErrNotFound):
		return nil, fmt.Errorf("記録を読めません: %w", err)
	}

	// 正本に記録が無い。手元に同じ session_id の記録があれば、それを続けて最初の同期で預ける。
	if sid != "" {
		p, found, err := s.adapter.Locate(sid)
		if err != nil {
			return nil, err
		}
		if found {
			s.sessionID, s.path = sid, p
			return s.adapter.ResumeCommand(sid), nil
		}
	}

	id, err := s.adapter.NewSessionID()
	if err != nil {
		return nil, err
	}
	if id == "" {
		// agent が ID を自分で決めるので、起こした後に Discover で見つけて meta に残す。
		s.needPatch = true
		return s.adapter.NewCommand(""), nil
	}
	if sid != "" {
		id = sid
	} else if _, err := s.o.Client.PatchSession(ctx, s.o.Number, manager.Patch{SessionID: &id}); err != nil {
		return nil, fmt.Errorf("session_id を meta に残せません: %w", err)
	}
	p, err := s.adapter.PlacePath(id, nil)
	if err != nil {
		return nil, err
	}
	s.sessionID, s.path = id, p
	return s.adapter.NewCommand(id), nil
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
		id := s.sessionID
		if _, err := s.o.Client.PatchSession(ctx, s.o.Number, manager.Patch{SessionID: &id}); err != nil {
			s.logf("session_id を meta に残せません: %v", err)
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
	sum := sha256.Sum256(data)

	if !s.stolen && sum != s.lastSum {
		err := s.o.Client.PutTranscript(ctx, s.o.Number, s.o.Holder, s.generation, data)
		switch {
		case err == nil:
			s.lastSum = sum
		case manager.IsConflict(err):
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
		key, err := s.o.Client.CreateFork(ctx, s.o.Number, data)
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
	_, err := s.o.Client.RenewLease(ctx, s.o.Number, s.o.Holder, s.generation)
	switch {
	case err == nil:
	case manager.IsConflict(err):
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
	err := s.o.Client.ReleaseLease(ctx, s.o.Number, s.o.Holder, s.generation)
	switch {
	case err == nil:
	case manager.IsConflict(err):
		s.logf("番号 %d の貸し出しは他に取られていました", s.o.Number)
	default:
		s.logf("貸し出しを返せません: %v", err)
	}
}

// writeFile は途中で落ちても壊れた記録が残らないよう、一時ファイルから rename する。
func writeFile(p string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".hs-*.jsonl")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, p)
}
