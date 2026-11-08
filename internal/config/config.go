// Package config は hs の設定を環境変数から読む。
package config

import (
	"errors"
	"os"
	"strings"
)

// Config は Manager の場所と認証、この機械の holder。
type Config struct {
	URL          string
	ClientID     string
	ClientSecret string
	Holder       string
}

// FromEnv は環境変数から Config を作る。getenv と hostname は差し替えられる。
func FromEnv(getenv func(string) string, hostname func() (string, error)) (*Config, error) {
	c := &Config{
		URL:          strings.TrimRight(getenv("HISHAKU_URL"), "/"),
		ClientID:     getenv("HISHAKU_ACCESS_CLIENT_ID"),
		ClientSecret: getenv("HISHAKU_ACCESS_CLIENT_SECRET"),
		Holder:       getenv("HISHAKU_HOLDER"),
	}
	if c.URL == "" {
		return nil, errors.New("HISHAKU_URL が設定されていません")
	}
	if (c.ClientID == "") != (c.ClientSecret == "") {
		return nil, errors.New("HISHAKU_ACCESS_CLIENT_ID と HISHAKU_ACCESS_CLIENT_SECRET は両方とも設定してください")
	}
	if c.Holder == "" {
		h, err := hostname()
		if err != nil {
			return nil, err
		}
		c.Holder = HolderFromHostname(h)
		if c.Holder == "" {
			return nil, errors.New("ホスト名から holder を作れません。HISHAKU_HOLDER を設定してください")
		}
	}
	return c, nil
}

// Load は実際の環境から Config を作る。
func Load() (*Config, error) {
	return FromEnv(os.Getenv, os.Hostname)
}

// HolderFromHostname はホスト名の最初のラベルを小文字の英数字とハイフンにする。
// macOS のホスト名は ".local" が付いたり付かなかったりするので、ドメイン部を落として揺れを消す。
func HolderFromHostname(h string) string {
	h, _, _ = strings.Cut(strings.ToLower(h), ".")
	var b strings.Builder
	for _, r := range h {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}
