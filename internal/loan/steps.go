package loan

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/ochanuco/hishaku/internal/manager"
)

// hs prepare・push・renew は別々のプロセスで呼ばれるので、prepare で分かったことを
// StateDir の <number>.json に残して push で使う。
// Codex の新規のセッションは起こした後でないと ID が分からず、
// どの rollout が今回のものかは prepare の時刻より後に書かれたかで見分けるしかないため。
type state struct {
	Generation int64     `json:"generation"`
	PreparedAt time.Time `json:"prepared_at"`
	// Sum は最後に正本と揃えた記録の hash。変わっていなければ PUT しない。
	Sum string `json:"sum"`
}

func statePath(dir string, number int) string {
	return filepath.Join(dir, strconv.Itoa(number)+".json")
}

func readState(dir string, number int, generation int64) *state {
	if dir == "" {
		return nil
	}
	b, err := os.ReadFile(statePath(dir, number))
	if err != nil {
		return nil
	}
	var st state
	if json.Unmarshal(b, &st) != nil || st.Generation != generation {
		return nil
	}
	return &st
}

func writeState(dir string, number int, st *state) error {
	if dir == "" {
		return nil
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return writeFile(statePath(dir, number), b)
}

func sumHex(data []byte) string {
	s := sha256.Sum256(data)
	return hex.EncodeToString(s[:])
}

// PrepareOptions は hs prepare の入力。
type PrepareOptions struct {
	Client   *manager.Client
	Number   int
	Holder   string
	Takeover bool
	TTL      time.Duration
	Adapter  AdapterFor
	// StateDir は push に引き継ぐ状態の置き場所。空なら残さない。
	StateDir string
	Now      func() time.Time
}

// Prepared は hs prepare の結果。呼ぶ側はこれで agent のコマンドを組み立てる。
type Prepared struct {
	Number         int    `json:"number"`
	Agent          string `json:"agent"`
	SessionID      string `json:"session_id"`
	Resume         bool   `json:"resume"`
	Generation     int64  `json:"generation"`
	LeaseExpiresAt string `json:"lease_expires_at"`
}

// Prepare は貸し出しを受けて正本の記録を置く。他が借りていれば HeldError。
func Prepare(ctx context.Context, o PrepareOptions) (*Prepared, error) {
	now := o.Now
	if now == nil {
		now = time.Now
	}
	startedAt := now()
	l, err := Acquire(ctx, o.Client, o.Number, o.Holder, o.Takeover, o.TTL)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Prepared, error) {
		rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = l.Release(rctx)
		return nil, err
	}
	p, err := l.Place(ctx, o.Adapter)
	if err != nil {
		return fail(err)
	}
	st := &state{Generation: l.Generation, PreparedAt: startedAt}
	if p.Sum != ([32]byte{}) {
		st.Sum = hex.EncodeToString(p.Sum[:])
	}
	if err := writeState(o.StateDir, o.Number, st); err != nil {
		return fail(fmt.Errorf("状態を残せません: %w", err))
	}
	return &Prepared{
		Number:         o.Number,
		Agent:          p.Agent.Name(),
		SessionID:      p.Session,
		Resume:         p.Resume,
		Generation:     l.Generation,
		LeaseExpiresAt: l.LeaseExpiresAt,
	}, nil
}

// PushOptions は hs push の入力。
type PushOptions struct {
	Client     *manager.Client
	Number     int
	Holder     string
	Generation int64
	Release    bool
	Adapter    AdapterFor
	StateDir   string
	// Log は hs 自身の知らせを書く先。
	Log io.Writer
}

// Push は agent の記録を見つけて正本に預け、Release なら貸し出しを返す。
// 取られていれば記録を forks に預けて ErrStolen を返す。
func Push(ctx context.Context, o PushOptions) error {
	if o.Log == nil {
		o.Log = io.Discard
	}
	l := &Loan{Client: o.Client, Number: o.Number, Holder: o.Holder, Generation: o.Generation}
	meta, err := o.Client.GetSession(ctx, o.Number)
	if err != nil {
		return fmt.Errorf("番号 %d の meta を読めません: %w", o.Number, err)
	}
	a, err := o.Adapter(meta.Agent)
	if err != nil {
		return err
	}
	st := readState(o.StateDir, o.Number, o.Generation)

	sid := manager.Str(meta.SessionID)
	var path string
	if sid != "" {
		p, found, err := a.Locate(sid)
		if err != nil {
			return err
		}
		if found {
			path = p
		}
	}
	if path == "" && st != nil {
		id, p, found, err := a.Discover(st.PreparedAt)
		if err != nil {
			return fmt.Errorf("記録を探せません: %w", err)
		}
		if found {
			path = p
			if id != sid {
				if err := l.SetSessionID(ctx, id); err != nil {
					return err
				}
			}
		}
	}
	if path == "" && sid == "" && st == nil {
		return fmt.Errorf("番号 %d の記録を探せません。この機械で hs prepare した世代 %d の状態がありません", o.Number, o.Generation)
	}

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("記録を読めません: %w", err)
		}
		// Manager は長さ 0 の本文を 411 で断るうえ、空の記録は預ける中身も無い。
		if len(data) > 0 && (st == nil || st.Sum != sumHex(data)) {
			if err := l.Put(ctx, data); err != nil {
				if errors.Is(err, ErrStolen) {
					key, ferr := l.Fork(ctx, data)
					if ferr != nil {
						return fmt.Errorf("%w。記録を forks にも預けられません: %v", err, ferr)
					}
					fmt.Fprintf(o.Log, "hs: 番号 %d の貸し出しは他に取られていました。記録を forks に預けました: %s\n", o.Number, key)
					return err
				}
				return fmt.Errorf("記録を預けられません: %w", err)
			}
			if st != nil {
				st.Sum = sumHex(data)
				if err := writeState(o.StateDir, o.Number, st); err != nil {
					fmt.Fprintf(o.Log, "hs: 状態を残せません: %v\n", err)
				}
			}
		}
	}

	if !o.Release {
		return nil
	}
	if err := l.Release(ctx); err != nil {
		if errors.Is(err, ErrStolen) {
			return err
		}
		return fmt.Errorf("貸し出しを返せません: %w", err)
	}
	if o.StateDir != "" {
		_ = os.Remove(statePath(o.StateDir, o.Number))
	}
	return nil
}

// Renew は貸し出しを延ばす。取られていれば ErrStolen。
func Renew(ctx context.Context, c *manager.Client, number int, holder string, generation int64) error {
	l := &Loan{Client: c, Number: number, Holder: holder, Generation: generation}
	if err := l.Renew(ctx); err != nil {
		if errors.Is(err, ErrStolen) {
			return err
		}
		return fmt.Errorf("貸し出しを延ばせません: %w", err)
	}
	return nil
}
