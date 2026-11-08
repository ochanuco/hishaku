package manager_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ochanuco/hishaku/internal/manager"
	"github.com/ochanuco/hishaku/internal/manager/managertest"
)

func TestClient(t *testing.T) {
	f := managertest.New(map[int]*managertest.Session{
		4821: {Agent: "claude", SessionID: "sid", Holder: "other-box", Transcript: []byte("a\n")},
	})
	defer f.Close()
	c := manager.New(f.Server.URL+"/", "client-id.example", "client-secret-example")
	ctx := context.Background()

	m, err := c.GetSession(ctx, 4821)
	if err != nil || m.Agent != "claude" || manager.Str(m.SessionID) != "sid" || m.Repo != nil {
		t.Fatalf("meta = %+v, %v", m, err)
	}
	calls := f.CallsTo("GET", "/v1/sessions/4821")
	if h := calls[0].Header; h.Get("CF-Access-Client-Id") != "client-id.example" || h.Get("CF-Access-Client-Secret") != "client-secret-example" {
		t.Fatalf("access headers = %v", h)
	}

	if _, err := c.GetSession(ctx, 1111); !errors.Is(err, manager.ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}

	_, err = c.AcquireLease(ctx, 4821, "box-a", manager.Acquire, time.Minute)
	var ce *manager.ConflictError
	if !errors.As(err, &ce) || ce.Code != manager.CodeLeaseHeld || ce.Holder != "other-box" || ce.LeaseExpiresAt == "" {
		t.Fatalf("acquire conflict = %v", err)
	}
	l, err := c.AcquireLease(ctx, 4821, "box-a", manager.Takeover, time.Minute)
	if err != nil || l.Generation != 1 {
		t.Fatalf("takeover = %+v, %v", l, err)
	}
	if body := f.CallsTo("POST", "/lease")[1].Body; body != `{"holder":"box-a","mode":"takeover","ttl_seconds":60}` {
		t.Fatalf("lease body = %s", body)
	}

	data, gen, err := c.GetTranscript(ctx, 4821)
	if err != nil || string(data) != "a\n" || gen != 1 {
		t.Fatalf("transcript = %q %d %v", data, gen, err)
	}
	if err := c.PutTranscript(ctx, 4821, "box-a", 1, []byte("a\nb\n")); err != nil {
		t.Fatal(err)
	}
	if q := f.CallsTo("PUT", "/transcript")[0].Query; q != "generation=1&holder=box-a" {
		t.Fatalf("put query = %s", q)
	}
	err = c.PutTranscript(ctx, 4821, "box-a", 2, []byte("x\n"))
	if !errors.As(err, &ce) || ce.Code != manager.CodeLeaseMismatch || ce.Holder != "box-a" || ce.Generation != 1 {
		t.Fatalf("stale put = %v", err)
	}
	if ct := f.CallsTo("PUT", "/transcript")[0].Header.Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("put content-type = %q", ct)
	}
	if _, err := c.RenewLease(ctx, 4821, "box-a", 1); err != nil {
		t.Fatal(err)
	}
	key, err := c.CreateFork(ctx, 4821, "box-a", []byte("x\n"))
	if err != nil || key == "" {
		t.Fatalf("fork = %q %v", key, err)
	}
	if q := f.CallsTo("POST", "/forks")[0].Query; q != "holder=box-a" {
		t.Fatalf("fork query = %s", q)
	}
	sid := "new-sid"
	if m, err := c.PatchSession(ctx, 4821, manager.Patch{SessionID: &sid}); err != nil || manager.Str(m.SessionID) != sid {
		t.Fatalf("patch = %+v %v", m, err)
	}
	if body := f.CallsTo("PATCH", "/4821")[0].Body; body != `{"session_id":"new-sid"}` {
		t.Fatalf("patch body = %s", body)
	}
	if err := c.ReleaseLease(ctx, 4821, "box-a", 1); err != nil {
		t.Fatal(err)
	}
	if body := f.CallsTo("DELETE", "/lease")[0].Body; body != `{"generation":1,"holder":"box-a"}` {
		t.Fatalf("release body = %s", body)
	}
	// 返した後の lease_mismatch は holder と lease_expires_at が null で返る。
	err = c.ReleaseLease(ctx, 4821, "box-a", 1)
	if !errors.As(err, &ce) || ce.Code != manager.CodeLeaseMismatch || ce.Holder != "" || ce.LeaseExpiresAt != "" {
		t.Fatalf("second release = %v", err)
	}
}
