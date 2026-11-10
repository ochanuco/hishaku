// Package report は hs ask と hs status の報告を、席を動かしている側（受付）に送る。
// hs は受付が何者かを知らず、HISHAKU_REPORT_URL に決まった形の JSON を POST するだけ。
package report

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Kind は報告の種類。
type Kind string

const (
	// Ask は持ち主への質問。答えは次のターンの指示として届く。
	Ask Kind = "ask"
	// Status はターンの途中の知らせ。
	Status Kind = "status"
)

// Report は受付に送る 1 件。
type Report struct {
	Number  int    `json:"number"`
	Type    Kind   `json:"type"`
	Subject string `json:"subject,omitempty"`
	Body    string `json:"body"`
}

// Target は送り先。Token があれば Bearer で付ける。
type Target struct {
	URL   string
	Token string
	HTTP  *http.Client
}

// FromEnv は環境変数から送り先を作る。
func FromEnv(getenv func(string) string) (*Target, error) {
	u := getenv("HISHAKU_REPORT_URL")
	if u == "" {
		return nil, errors.New("HISHAKU_REPORT_URL が設定されていません。報告は hs turn で走らせた席からだけ送れます")
	}
	return &Target{URL: u, Token: getenv("HISHAKU_REPORT_TOKEN")}, nil
}

// Send は報告を POST する。2xx 以外は error。
func (t *Target) Send(ctx context.Context, r Report) error {
	if strings.TrimSpace(r.Body) == "" {
		return errors.New("本文が空です")
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.URL, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if t.Token != "" {
		req.Header.Set("Authorization", "Bearer "+t.Token)
	}
	c := t.HTTP
	if c == nil {
		c = http.DefaultClient
	}
	res, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("受付に送れません: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 1024))
		return fmt.Errorf("受付が %d で断りました: %s", res.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}
