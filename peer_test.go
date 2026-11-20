package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"
)

type fakeWhoIs struct{ resp *apitype.WhoIsResponse }

func (f fakeWhoIs) WhoIs(context.Context, string) (*apitype.WhoIsResponse, error) {
	if f.resp == nil {
		return nil, errors.New("not a tailnet address")
	}
	return f.resp, nil
}

func who(node, login string, tags ...string) *apitype.WhoIsResponse {
	return &apitype.WhoIsResponse{
		Node:        &tailcfg.Node{ComputedName: node, Tags: tags},
		UserProfile: &tailcfg.UserProfile{LoginName: login},
	}
}

func newTestRelay(t *testing.T, w *apitype.WhoIsResponse) (*Relay, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	cfg := &Config{
		Name: "laptop",
		Peers: map[string]Peer{
			"linux-mini": {Host: "relay-linux-mini", Login: "me@example.com"},
			"alice":      {Host: "relay-alice", Tag: "tag:agent-alice"},
		},
		Workspace: dir,
	}
	return &Relay{cfg: cfg, store: store, whois: fakeWhoIs{w}, limiter: newLimiter(2, time.Minute)}, cfg.InboxDir()
}

const msgID = "c1698db1-951d-4820-b1e3-e59eb1067dab"

func TestReceiveAuthorization(t *testing.T) {
	tests := []struct {
		name     string
		who      *apitype.WhoIsResponse
		body     string
		wantCode int
		wantFrom string
	}{
		{"peer login", who("relay-linux-mini", "me@example.com"), `{"id":"` + msgID + `","body":"hi"}`, http.StatusAccepted, "linux-mini"},
		{"shared-in fqdn", who("relay-linux-mini.tail05b44.ts.net", "me@example.com"), `{"id":"` + msgID + `","body":"hi"}`, http.StatusAccepted, "linux-mini"},
		{"peer tag", who("relay-alice", "tagged-devices", "tag:agent-alice"), `{"id":"` + msgID + `","body":"hi"}`, http.StatusAccepted, "alice"},
		{"unknown node with a known login", who("other-box", "me@example.com"), `{"id":"` + msgID + `","body":"hi"}`, http.StatusForbidden, ""},
		{"known host, foreign login", who("relay-linux-mini", "eve@example.com"), `{"id":"` + msgID + `","body":"hi"}`, http.StatusForbidden, ""},
		{"friend takes another peer's hostname", who("relay-linux-mini", "tagged-devices", "tag:agent-alice"), `{"id":"` + msgID + `","body":"hi"}`, http.StatusForbidden, ""},
		{"right host, wrong tag", who("relay-alice", "tagged-devices", "tag:agent-bob"), `{"id":"` + msgID + `","body":"hi"}`, http.StatusForbidden, ""},
		{"tag peer with a login", who("relay-alice", "me@example.com"), `{"id":"` + msgID + `","body":"hi"}`, http.StatusForbidden, ""},
		{"not on tailnet", nil, `{"id":"` + msgID + `","body":"hi"}`, http.StatusForbidden, ""},
		{"from field is ignored", who("relay-linux-mini", "me@example.com"), `{"id":"` + msgID + `","from":"alice","body":"hi"}`, http.StatusAccepted, "linux-mini"},
		{"bad id", who("relay-linux-mini", "me@example.com"), `{"id":"nope","body":"hi"}`, http.StatusBadRequest, ""},
		{"empty body", who("relay-linux-mini", "me@example.com"), `{"id":"` + msgID + `","body":""}`, http.StatusBadRequest, ""},
		{"bad card", who("relay-linux-mini", "me@example.com"), `{"id":"` + msgID + `","body":"hi","card":"x"}`, http.StatusBadRequest, ""},
		{"oversized body", who("relay-linux-mini", "me@example.com"), `{"id":"` + msgID + `","body":"` + strings.Repeat("x", maxBody+1) + `"}`, http.StatusBadRequest, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, inbox := newTestRelay(t, tt.who)
			rec := httptest.NewRecorder()
			r.PeerHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(tt.body)))
			if rec.Code != tt.wantCode {
				t.Fatalf("code = %d, want %d (%s)", rec.Code, tt.wantCode, rec.Body)
			}
			msgs, err := r.store.TakeInbox(context.Background(), false, 10, "")
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantFrom == "" {
				if len(msgs) != 0 {
					t.Fatalf("stored %d messages, want none", len(msgs))
				}
				return
			}
			if len(msgs) != 1 || msgs[0].From != tt.wantFrom {
				t.Fatalf("stored %+v, want one from %q", msgs, tt.wantFrom)
			}
			if _, err := os.Stat(filepath.Join(inbox, msgID+".json")); err != nil {
				t.Fatalf("doorbell missing: %v", err)
			}
		})
	}
}

func TestReceiveIdempotentAndRateLimited(t *testing.T) {
	r, _ := newTestRelay(t, who("relay-linux-mini", "me@example.com"))
	post := func(id string) int {
		rec := httptest.NewRecorder()
		r.PeerHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages",
			strings.NewReader(`{"id":"`+id+`","body":"hi"}`)))
		return rec.Code
	}
	codes := []int{post(msgID), post(msgID), post("358e7970-e82e-430a-871b-afb1a1c7d8f7")}
	want := []int{http.StatusAccepted, http.StatusAccepted, http.StatusTooManyRequests}
	for i := range want {
		if codes[i] != want[i] {
			t.Fatalf("codes = %v, want %v", codes, want)
		}
	}
	first, _ := r.store.TakeInbox(context.Background(), false, 10, "")
	again, _ := r.store.TakeInbox(context.Background(), false, 10, "")
	if len(first) != 1 || len(again) != 0 {
		t.Fatalf("retry stored %d, second read %d; want 1 then 0", len(first), len(again))
	}
}

const cardID = "01a1f963-d635-7cbe-82b0-d6fe93dde960"

func TestCardMessagesAndNotify(t *testing.T) {
	got := make(chan map[string]string, 1)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var m map[string]string
		_ = json.NewDecoder(req.Body).Decode(&m)
		got <- m
	}))
	defer app.Close()
	r, _ := newTestRelay(t, who("relay-linux-mini", "me@example.com"))
	r.cfg.Notify = app.URL
	for _, body := range []string{`{"id":"` + msgID + `","body":"about the card","card":"` + cardID + `"}`, `{"id":"358e7970-e82e-430a-871b-afb1a1c7d8f7","body":"plain"}`} {
		rec := httptest.NewRecorder()
		r.PeerHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("code = %d (%s)", rec.Code, rec.Body)
		}
	}
	select {
	case m := <-got:
		if m["id"] != msgID || m["from"] != "linux-mini" || m["card"] != cardID || m["body"] != "" {
			t.Fatalf("notified %v", m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("app not notified")
	}
	about, _ := r.store.TakeInbox(context.Background(), false, 10, cardID)
	rest, _ := r.store.TakeInbox(context.Background(), false, 10, "")
	if len(about) != 1 || about[0].Card != cardID || len(rest) != 1 || rest[0].Card != "" {
		t.Fatalf("about card %+v, rest %+v", about, rest)
	}
}

func TestOldStoreGetsCardColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE messages (id TEXT PRIMARY KEY, direction TEXT NOT NULL, peer TEXT NOT NULL, login TEXT,
		body TEXT NOT NULL, created_at TEXT NOT NULL, read_at TEXT)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Insert(context.Background(), msgID, "in", "linux-mini", "hi", "", cardID); err != nil {
		t.Fatal(err)
	}
}

func TestCheckNotify(t *testing.T) {
	for raw, ok := range map[string]bool{
		"": true, "http://127.0.0.1:4100/api/agent-relay": true, "http://localhost:9/x": true,
		"https://127.0.0.1/x": false, "http://example.com/x": false, "http://10.0.0.2/x": false,
	} {
		if err := checkNotify(raw); (err == nil) != ok {
			t.Errorf("checkNotify(%q) = %v, want ok=%v", raw, err, ok)
		}
	}
}
