package report

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSend(t *testing.T) {
	var got Report
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	tg := &Target{URL: srv.URL, Token: "tok"}
	if err := tg.Send(context.Background(), Report{Number: 4821, Type: Ask, Subject: "件名", Body: "聞きたいこと"}); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer tok" || got != (Report{Number: 4821, Type: Ask, Subject: "件名", Body: "聞きたいこと"}) {
		t.Fatalf("auth = %q got = %+v", auth, got)
	}
}

func TestSendRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "知らない番号です", http.StatusNotFound)
	}))
	defer srv.Close()

	err := (&Target{URL: srv.URL}).Send(context.Background(), Report{Number: 4821, Type: Status, Body: "途中"})
	if err == nil || !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "知らない番号です") {
		t.Fatalf("err = %v", err)
	}
}

func TestSendEmptyBody(t *testing.T) {
	if err := (&Target{URL: "http://example.com"}).Send(context.Background(), Report{Number: 4821, Type: Status, Body: " \n"}); err == nil {
		t.Fatal("空の本文を送りました")
	}
}

func TestFromEnvNeedsURL(t *testing.T) {
	if _, err := FromEnv(func(string) string { return "" }); err == nil {
		t.Fatal("URL なしで作れました")
	}
}
