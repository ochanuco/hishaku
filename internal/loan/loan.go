// Package loan は番号の貸し出しを受けて記録を置き、預け、返すまでの手順を持つ。
// hs resume と、呼ぶ側が agent を自分で起こす hs prepare・push・renew が共有する。
package loan

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ochanuco/hishaku/internal/agent"
	"github.com/ochanuco/hishaku/internal/manager"
)

// DefaultTTL は貸し出しの期限。延ばす間隔（30 秒）より十分長くする。
const DefaultTTL = 2 * time.Minute

// ErrStolen は今の貸し出しが他に取られていたこと（409）を表す。
var ErrStolen = errors.New("貸し出しは他に取られていました")

// HeldError は他の holder が借りていて acquire できなかったことを表す。
type HeldError struct {
	Number         int
	Holder         string
	LeaseExpiresAt string
}

func (e *HeldError) Error() string {
	return fmt.Sprintf("番号 %d は %s が %s まで借りています（--takeover で奪えます）", e.Number, e.Holder, e.LeaseExpiresAt)
}

// AdapterFor は meta の agent の名前から Adapter を作る。
type AdapterFor func(name string) (agent.Adapter, error)

// Loan は受けた 1 つの貸し出し。
type Loan struct {
	Client         *manager.Client
	Number         int
	Holder         string
	Generation     int64
	LeaseExpiresAt string
}

// Acquire は貸し出しを受ける。他が借りていれば HeldError。
func Acquire(ctx context.Context, c *manager.Client, number int, holder string, takeover bool, ttl time.Duration) (*Loan, error) {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	mode := manager.Acquire
	if takeover {
		mode = manager.Takeover
	}
	lease, err := c.AcquireLease(ctx, number, holder, mode, ttl)
	if err != nil {
		var ce *manager.ConflictError
		if errors.As(err, &ce) {
			return nil, &HeldError{Number: number, Holder: ce.Holder, LeaseExpiresAt: ce.LeaseExpiresAt}
		}
		return nil, fmt.Errorf("貸し出しを受けられません: %w", err)
	}
	return &Loan{Client: c, Number: number, Holder: holder, Generation: lease.Generation, LeaseExpiresAt: lease.LeaseExpiresAt}, nil
}

func stolen(err error) error {
	if manager.IsConflict(err) {
		return fmt.Errorf("%w: %v", ErrStolen, err)
	}
	return err
}

// Put は今の貸し出しのもとで正本の記録を置き換える。取られていれば ErrStolen。
func (l *Loan) Put(ctx context.Context, data []byte) error {
	return stolen(l.Client.PutTranscript(ctx, l.Number, l.Holder, l.Generation, data))
}

// Fork は取られた側の記録を枝として預け、その key を返す。
func (l *Loan) Fork(ctx context.Context, data []byte) (string, error) {
	return l.Client.CreateFork(ctx, l.Number, l.Holder, data)
}

// Renew は貸し出しを延ばす。取られていれば ErrStolen。
func (l *Loan) Renew(ctx context.Context) error {
	lease, err := l.Client.RenewLease(ctx, l.Number, l.Holder, l.Generation)
	if err != nil {
		return stolen(err)
	}
	l.LeaseExpiresAt = lease.LeaseExpiresAt
	return nil
}

// Release は貸し出しを返す。取られていれば ErrStolen。
func (l *Loan) Release(ctx context.Context) error {
	return stolen(l.Client.ReleaseLease(ctx, l.Number, l.Holder, l.Generation))
}

// SetSessionID は session_id を meta に残す。
func (l *Loan) SetSessionID(ctx context.Context, id string) error {
	if _, err := l.Client.PatchSession(ctx, l.Number, manager.Patch{SessionID: &id}); err != nil {
		return fmt.Errorf("session_id を meta に残せません: %w", err)
	}
	return nil
}

// Placed は Place が置いた記録と、agent の起こし方。
type Placed struct {
	Agent   agent.Adapter
	Resume  bool
	Session string
	// Path は記録の場所。空なら agent が ID を自分で決めるので、起こした後に Discover で探す。
	Path string
	// Sum は置いた記録の hash。何も置いていなければ零値。
	Sum [32]byte
}

// Command は agent を起こす argv。extra は agent に渡す追加の引数。
func (p *Placed) Command(extra []string) []string {
	if p.Resume {
		return p.Agent.ResumeCommand(p.Session, extra)
	}
	return p.Agent.NewCommand(p.Session, extra)
}

// Place は正本の記録を agent の置き場所に置く。正本が無ければ新規のセッションの用意をする。
func (l *Loan) Place(ctx context.Context, adapterFor AdapterFor) (*Placed, error) {
	meta, err := l.Client.GetSession(ctx, l.Number)
	if err != nil {
		return nil, fmt.Errorf("番号 %d の meta を読めません: %w", l.Number, err)
	}
	a, err := adapterFor(meta.Agent)
	if err != nil {
		return nil, err
	}
	sid := manager.Str(meta.SessionID)

	data, _, err := l.Client.GetTranscript(ctx, l.Number)
	switch {
	case err == nil:
		if sid == "" {
			return nil, fmt.Errorf("番号 %d は記録があるのに session_id がありません", l.Number)
		}
		p, err := a.PlacePath(sid, data)
		if err != nil {
			return nil, err
		}
		if err := writeFile(p, data); err != nil {
			return nil, fmt.Errorf("記録を置けません: %w", err)
		}
		return &Placed{Agent: a, Resume: true, Session: sid, Path: p, Sum: sha256.Sum256(data)}, nil
	case !errors.Is(err, manager.ErrNotFound):
		return nil, fmt.Errorf("記録を読めません: %w", err)
	}

	// 正本に記録が無い。手元に同じ session_id の記録があれば、それを続けて次に預ける。
	if sid != "" {
		p, found, err := a.Locate(sid)
		if err != nil {
			return nil, err
		}
		if found {
			return &Placed{Agent: a, Resume: true, Session: sid, Path: p}, nil
		}
	}

	id, err := a.NewSessionID()
	if err != nil {
		return nil, err
	}
	if id == "" {
		return &Placed{Agent: a}, nil
	}
	if sid != "" {
		id = sid
	} else if err := l.SetSessionID(ctx, id); err != nil {
		return nil, err
	}
	p, err := a.PlacePath(id, nil)
	if err != nil {
		return nil, err
	}
	return &Placed{Agent: a, Session: id, Path: p}, nil
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
