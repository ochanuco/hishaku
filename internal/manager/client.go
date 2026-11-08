// Package manager は Manager（Cloudflare Worker）の v1 API を呼ぶクライアント。
package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrNotFound は番号や記録が無いときに返る。
var ErrNotFound = errors.New("not found")

// 409 の本文の error。
const (
	CodeLeaseHeld     = "lease_held"
	CodeLeaseMismatch = "lease_mismatch"
	CodeLeaseFree     = "lease_free"
)

// ConflictError は 409 のときに返る。lease_held では持ち主と期限、lease_mismatch では今の持ち主・世代・期限が入る。
// lease_mismatch の holder と lease_expires_at は null のことがあり、そのときは空文字。
type ConflictError struct {
	Code           string
	Holder         string
	Generation     int64
	LeaseExpiresAt string
	Body           string
}

func (e *ConflictError) Error() string {
	if e.Holder != "" {
		return fmt.Sprintf("%s: held by %s until %s", e.Code, e.Holder, e.LeaseExpiresAt)
	}
	return "conflict: " + e.Code
}

// IsConflict は err が 409 由来かを返す。
func IsConflict(err error) bool {
	var c *ConflictError
	return errors.As(err, &c)
}

// Meta は番号ごとの meta。
type Meta struct {
	Number         int     `json:"number"`
	PostID         string  `json:"post_id"`
	Agent          string  `json:"agent"`
	SessionID      *string `json:"session_id"`
	Title          string  `json:"title"`
	Repo           *string `json:"repo"`
	Branch         *string `json:"branch"`
	Holder         *string `json:"holder"`
	Generation     int64   `json:"generation"`
	LeaseExpiresAt *string `json:"lease_expires_at"`
	// ReleaseRequestedBy は今の持ち主に返してほしいと頼んだ Commander。
	ReleaseRequestedBy *string `json:"release_requested_by"`
	// PushedAt は最後に記録を PUT した時刻。
	PushedAt  *string `json:"pushed_at"`
	ExpiresAt string  `json:"expires_at"`
	UpdatedAt string  `json:"updated_at"`
}

// Str は nil を空文字として読む。
func Str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// Patch は PATCH /v1/sessions/{number} で変える項目。nil は送らない。
type Patch struct {
	Title     *string `json:"title,omitempty"`
	Repo      *string `json:"repo,omitempty"`
	Branch    *string `json:"branch,omitempty"`
	SessionID *string `json:"session_id,omitempty"`
}

// Lease は借りたとき・延ばしたときの世代と期限。延ばしたときは、返してほしいと頼んだ Commander も入る。
type Lease struct {
	Generation         int64   `json:"generation"`
	LeaseExpiresAt     string  `json:"lease_expires_at"`
	ReleaseRequestedBy *string `json:"release_requested_by"`
}

// ReleaseRequest は返してほしいと頼んだときの、今の貸し出し。
type ReleaseRequest struct {
	Holder             string `json:"holder"`
	Generation         int64  `json:"generation"`
	LeaseExpiresAt     string `json:"lease_expires_at"`
	ReleaseRequestedBy string `json:"release_requested_by"`
}

// LeaseMode は acquire（空いているときだけ）か takeover（奪う）。
type LeaseMode string

const (
	Acquire  LeaseMode = "acquire"
	Takeover LeaseMode = "takeover"
)

// Client は Manager への HTTP クライアント。
type Client struct {
	BaseURL      string
	ClientID     string
	ClientSecret string
	HTTP         *http.Client
}

// New は既定の http.Client で Client を作る。
func New(baseURL, clientID, clientSecret string) *Client {
	return &Client{
		BaseURL:      strings.TrimRight(baseURL, "/"),
		ClientID:     clientID,
		ClientSecret: clientSecret,
		HTTP:         &http.Client{Timeout: 60 * time.Second},
	}
}

func (c *Client) path(number int, rest string) string {
	return fmt.Sprintf("/v1/sessions/%d%s", number, rest)
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body io.Reader, contentType string) (*http.Response, []byte, error) {
	u := c.BaseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.ClientID != "" {
		req.Header.Set("CF-Access-Client-Id", c.ClientID)
		req.Header.Set("CF-Access-Client-Secret", c.ClientSecret)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	res, err := hc.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, nil, err
	}
	switch {
	case res.StatusCode == http.StatusNotFound:
		return res, data, ErrNotFound
	case res.StatusCode == http.StatusConflict:
		ce := &ConflictError{Body: string(data)}
		var v struct {
			Error          string  `json:"error"`
			Holder         *string `json:"holder"`
			Generation     int64   `json:"generation"`
			LeaseExpiresAt *string `json:"lease_expires_at"`
		}
		if json.Unmarshal(data, &v) == nil {
			ce.Code, ce.Holder, ce.Generation, ce.LeaseExpiresAt = v.Error, Str(v.Holder), v.Generation, Str(v.LeaseExpiresAt)
		}
		return res, data, ce
	case res.StatusCode < 200 || res.StatusCode >= 300:
		return res, data, fmt.Errorf("%s %s: %s: %s", method, path, res.Status, strings.TrimSpace(string(data)))
	}
	return res, data, nil
}

func (c *Client) doJSON(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	ct := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, ct = bytes.NewReader(b), "application/json"
	}
	_, data, err := c.do(ctx, method, path, nil, body, ct)
	if err != nil {
		return err
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("%s %s: decode: %w", method, path, err)
		}
	}
	return nil
}

// GetSession は meta を返す。
func (c *Client) GetSession(ctx context.Context, number int) (*Meta, error) {
	var m Meta
	if err := c.doJSON(ctx, http.MethodGet, c.path(number, ""), nil, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// PatchSession は meta の一部を更新する。
func (c *Client) PatchSession(ctx context.Context, number int, p Patch) (*Meta, error) {
	var m Meta
	if err := c.doJSON(ctx, http.MethodPatch, c.path(number, ""), p, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// GetTranscript は正本の記録と今の世代を返す。無ければ ErrNotFound。
func (c *Client) GetTranscript(ctx context.Context, number int) ([]byte, int64, error) {
	res, data, err := c.do(ctx, http.MethodGet, c.path(number, "/transcript"), nil, nil, "")
	if err != nil {
		return nil, 0, err
	}
	gen, _ := strconv.ParseInt(res.Header.Get("X-Hishaku-Generation"), 10, 64)
	return data, gen, nil
}

// PutTranscript は今の貸し出しのもとで記録を置き換える。合わなければ ConflictError。
func (c *Client) PutTranscript(ctx context.Context, number int, holder string, generation int64, data []byte) error {
	q := url.Values{"holder": {holder}, "generation": {strconv.FormatInt(generation, 10)}}
	_, _, err := c.do(ctx, http.MethodPut, c.path(number, "/transcript"), q, bytes.NewReader(data), "application/x-ndjson")
	return err
}

// CreateFork は取られた側の記録を枝として残し、その key を返す。holder は預けた側として記録に残る。
func (c *Client) CreateFork(ctx context.Context, number int, holder string, data []byte) (string, error) {
	q := url.Values{"holder": {holder}}
	_, body, err := c.do(ctx, http.MethodPost, c.path(number, "/forks"), q, bytes.NewReader(data), "application/x-ndjson")
	if err != nil {
		return "", err
	}
	var v struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return "", fmt.Errorf("forks: decode: %w", err)
	}
	return v.Key, nil
}

// AcquireLease は貸し出しを受ける。持ち主がいれば ConflictError（Holder と期限入り）。
func (c *Client) AcquireLease(ctx context.Context, number int, holder string, mode LeaseMode, ttl time.Duration) (*Lease, error) {
	in := map[string]any{"holder": holder, "mode": mode, "ttl_seconds": int(ttl / time.Second)}
	var l Lease
	if err := c.doJSON(ctx, http.MethodPost, c.path(number, "/lease"), in, &l); err != nil {
		return nil, err
	}
	return &l, nil
}

// RenewLease は期限を延ばし、新しい期限を返す。
func (c *Client) RenewLease(ctx context.Context, number int, holder string, generation int64) (*Lease, error) {
	in := map[string]any{"holder": holder, "generation": generation}
	var l Lease
	if err := c.doJSON(ctx, http.MethodPost, c.path(number, "/lease/renew"), in, &l); err != nil {
		return nil, err
	}
	return &l, nil
}

// ReleaseLease は貸し出しを返す。
func (c *Client) ReleaseLease(ctx context.Context, number int, holder string, generation int64) error {
	in := map[string]any{"holder": holder, "generation": generation}
	return c.doJSON(ctx, http.MethodDelete, c.path(number, "/lease"), in, nil)
}

// RequestRelease は今の持ち主に返してほしいと頼む。誰も持っていなければ lease_free、
// 頼んだ側が今の持ち主なら lease_mismatch の ConflictError。
func (c *Client) RequestRelease(ctx context.Context, number int, holder string) (*ReleaseRequest, error) {
	in := map[string]any{"holder": holder}
	var r ReleaseRequest
	if err := c.doJSON(ctx, http.MethodPost, c.path(number, "/lease/request"), in, &r); err != nil {
		return nil, err
	}
	return &r, nil
}
