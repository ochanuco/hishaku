// Package managertest は Manager の v1 API を真似る偽物を持つ。テスト専用。
package managertest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Session は偽の Manager が持つ 1 番号ぶんの状態。
type Session struct {
	Agent      string
	SessionID  string
	Title      string
	Repo       string
	Branch     string
	Holder     string
	Generation int64
	Transcript []byte // nil なら記録なし
	Forks      [][]byte
}

// Call は受けたリクエストの記録。
type Call struct {
	Method string
	Path   string
	Query  string
	Body   string
	Header http.Header
}

// Fake は httptest の上で動く偽の Manager。
type Fake struct {
	mu       sync.Mutex
	Sessions map[int]*Session
	Calls    []Call
	// AfterPut は PUT が通った後に呼ばれる。取られた状況を作るのに使う。
	AfterPut func(s *Session)
	Server   *httptest.Server
}

// New は偽の Manager を立てる。
func New(sessions map[int]*Session) *Fake {
	f := &Fake{Sessions: sessions}
	f.Server = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}

// Close は止める。
func (f *Fake) Close() { f.Server.Close() }

// Lock は状態を読むときに使う。
func (f *Fake) Lock() func() {
	f.mu.Lock()
	return f.mu.Unlock
}

// CallsTo は method と path の末尾が合う呼び出しを返す。
func (f *Fake) CallsTo(method, suffix string) []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Call
	for _, c := range f.Calls {
		if c.Method == method && strings.HasSuffix(c.Path, suffix) {
			out = append(out, c)
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (f *Fake) meta(n int, s *Session) map[string]any {
	return map[string]any{
		"number": n, "post_id": "post-1", "agent": s.Agent, "session_id": nullable(s.SessionID),
		"title": s.Title, "repo": nullable(s.Repo), "branch": nullable(s.Branch), "holder": nullable(s.Holder),
		"generation": s.Generation, "lease_expires_at": "2026-01-01T00:10:00Z",
		"expires_at": "2026-02-01T00:00:00Z", "updated_at": "2026-01-01T00:00:00Z",
	}
}

func (f *Fake) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, Call{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Body: string(body), Header: r.Header.Clone()})

	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 3 || parts[0] != "v1" || parts[1] != "sessions" {
		http.NotFound(w, r)
		return
	}
	n, _ := strconv.Atoi(parts[2])
	s, ok := f.Sessions[n]
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "not_found"})
		return
	}
	rest := strings.Join(parts[3:], "/")
	var in struct {
		Holder     string `json:"holder"`
		Mode       string `json:"mode"`
		Generation int64  `json:"generation"`
		SessionID  string `json:"session_id"`
	}
	_ = json.Unmarshal(body, &in)
	held := func(holder string, gen int64) bool { return s.Holder == holder && s.Generation == gen }

	switch {
	case rest == "" && r.Method == http.MethodGet:
		writeJSON(w, 200, f.meta(n, s))
	case rest == "" && r.Method == http.MethodPatch:
		if in.SessionID != "" {
			s.SessionID = in.SessionID
		}
		writeJSON(w, 200, f.meta(n, s))
	case rest == "transcript" && r.Method == http.MethodGet:
		if s.Transcript == nil {
			writeJSON(w, 404, map[string]string{"error": "not_found"})
			return
		}
		w.Header().Set("X-Hishaku-Generation", strconv.FormatInt(s.Generation, 10))
		_, _ = w.Write(s.Transcript)
	case rest == "transcript" && r.Method == http.MethodPut:
		gen, _ := strconv.ParseInt(r.URL.Query().Get("generation"), 10, 64)
		if !held(r.URL.Query().Get("holder"), gen) {
			writeJSON(w, 409, map[string]string{"error": "lease_mismatch"})
			return
		}
		s.Transcript = body
		if f.AfterPut != nil {
			f.AfterPut(s)
		}
		writeJSON(w, 200, map[string]any{"generation": s.Generation})
	case rest == "forks" && r.Method == http.MethodPost:
		s.Forks = append(s.Forks, body)
		writeJSON(w, 201, map[string]string{"key": fmt.Sprintf("sessions/%d/forks/fork-%d.jsonl", n, len(s.Forks))})
	case rest == "lease" && r.Method == http.MethodPost:
		if in.Mode == "acquire" && s.Holder != "" {
			writeJSON(w, 409, map[string]string{"holder": s.Holder, "lease_expires_at": "2026-01-01T00:10:00Z"})
			return
		}
		s.Holder = in.Holder
		s.Generation++
		writeJSON(w, 200, map[string]any{"generation": s.Generation, "lease_expires_at": time.Now().Add(time.Minute).UTC().Format(time.RFC3339)})
	case rest == "lease/renew" && r.Method == http.MethodPost:
		if !held(in.Holder, in.Generation) {
			writeJSON(w, 409, map[string]string{"error": "lease_mismatch"})
			return
		}
		writeJSON(w, 200, map[string]any{"generation": s.Generation, "lease_expires_at": time.Now().Add(time.Minute).UTC().Format(time.RFC3339)})
	case rest == "lease" && r.Method == http.MethodDelete:
		if !held(in.Holder, in.Generation) {
			writeJSON(w, 409, map[string]string{"error": "lease_mismatch"})
			return
		}
		s.Holder = ""
		w.WriteHeader(204)
	default:
		http.NotFound(w, r)
	}
}
