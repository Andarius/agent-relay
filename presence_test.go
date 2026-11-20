package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// toServer sends every request to one test server, whatever host it names
type toServer struct{ url *url.URL }

func (t toServer) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme, req.URL.Host = t.url.Scheme, t.url.Host
	return http.DefaultTransport.RoundTrip(req)
}

func TestPresenceIn(t *testing.T) {
	const ok = `{"agents":[{"card":"` + cardID + `","state":"waiting","question":"merge?"}]}`
	tests := []struct {
		name string
		who  bool
		body string
		want int
	}{
		{"peer", true, ok, http.StatusAccepted},
		{"empty list", true, `{"agents":[]}`, http.StatusAccepted},
		{"not a peer", false, ok, http.StatusForbidden},
		{"unknown state", true, `{"agents":[{"card":"` + cardID + `","state":"asleep"}]}`, http.StatusBadRequest},
		{"no card", true, `{"agents":[{"state":"idle"}]}`, http.StatusBadRequest},
		{"long question", true, `{"agents":[{"card":"` + cardID + `","state":"waiting","question":"` + strings.Repeat("q", maxQuestion+1) + `"}]}`, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			caller := who("relay-other", "eve@example.com")
			if tt.who {
				caller = who("relay-linux-mini", "me@example.com")
			}
			r, _ := newTestRelay(t, caller)
			rec := httptest.NewRecorder()
			r.PeerHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/presence", strings.NewReader(tt.body)))
			if rec.Code != tt.want {
				t.Fatalf("code = %d, want %d (%s)", rec.Code, tt.want, rec.Body)
			}
			_, stored := r.presence.all()["linux-mini"]
			if stored != (tt.want == http.StatusAccepted) {
				t.Fatalf("stored = %v", stored)
			}
		})
	}
}

func TestLocalPresenceSharesAndLists(t *testing.T) {
	peer, _ := newTestRelay(t, who("relay-alice", "tagged-devices", "tag:agent-alice"))
	srv := httptest.NewServer(peer.PeerHandler())
	defer srv.Close()
	target, _ := url.Parse(srv.URL)
	me, _ := newTestRelay(t, nil)
	me.cfg.MCPToken = strings.Repeat("t", 32)
	me.client = &http.Client{Transport: toServer{target}}
	local := me.LocalHandler()
	call := func(method, token, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/local/presence", strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		local.ServeHTTP(rec, req)
		return rec
	}
	if rec := call(http.MethodGet, "wrong", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad token: %d", rec.Code)
	}
	rec := call(http.MethodPost, me.cfg.MCPToken, `{"agents":[{"card":"`+cardID+`","state":"working"}]}`)
	var out struct {
		Sent []string `json:"sent"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusOK || len(out.Sent) != 2 {
		t.Fatalf("share: %d %s", rec.Code, rec.Body)
	}
	got := peer.presence.all()["alice"]
	if len(got.Agents) != 1 || got.Agents[0].State != "working" {
		t.Fatalf("peer holds %+v", got)
	}
	me.presence.set("linux-mini", []AgentStatus{{Card: cardID, State: "waiting"}})
	var list struct {
		Peers map[string]PeerPresence `json:"peers"`
	}
	_ = json.Unmarshal(call(http.MethodGet, me.cfg.MCPToken, "").Body.Bytes(), &list)
	if list.Peers["linux-mini"].Agents[0].State != "waiting" {
		t.Fatalf("list %+v", list)
	}
}
