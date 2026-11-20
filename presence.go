package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	maxAgents   = 100
	maxQuestion = 500
)

// AgentStatus is what one agent on a machine is doing about a Trame card.
type AgentStatus struct {
	Card     string `json:"card"`
	State    string `json:"state"` // working | waiting | idle
	Question string `json:"question,omitempty"`
	Since    string `json:"since,omitempty"`
}

type presenceIn struct {
	Agents []AgentStatus `json:"agents"`
}

// PeerPresence is the last list a peer sent, and when it arrived.
type PeerPresence struct {
	Agents     []AgentStatus `json:"agents"`
	ReceivedAt string        `json:"received_at"`
}

// presenceBook keeps the last status list per peer, in memory: statuses are stale within minutes anyway.
type presenceBook struct {
	mu    sync.Mutex
	peers map[string]PeerPresence
}

func (b *presenceBook) set(peer string, agents []AgentStatus) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.peers == nil {
		b.peers = map[string]PeerPresence{}
	}
	b.peers[peer] = PeerPresence{Agents: agents, ReceivedAt: now()}
}

func (b *presenceBook) all() map[string]PeerPresence {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]PeerPresence, len(b.peers))
	for k, v := range b.peers {
		out[k] = v
	}
	return out
}

func validAgents(agents []AgentStatus) bool {
	if len(agents) > maxAgents {
		return false
	}
	for _, a := range agents {
		if a.Card == "" || !validCard(a.Card) || len(a.Question) > maxQuestion || len(a.Since) > 40 {
			return false
		}
		if a.State != "working" && a.State != "waiting" && a.State != "idle" {
			return false
		}
	}
	return true
}

// receivePresence takes a peer's status list (POST /v1/presence), authorized like a message.
func (r *Relay) receivePresence(w http.ResponseWriter, req *http.Request) {
	who, err := r.whois.WhoIs(req.Context(), req.RemoteAddr)
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "unknown caller"})
		return
	}
	sender, _, ok := r.authorize(who)
	if !ok {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}
	var in presenceIn
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 128<<10)).Decode(&in); err != nil || !validAgents(in.Agents) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "expected {agents: [{card, state, question?, since?}]}"})
		return
	}
	if !r.presenceLimit().allow(sender) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limited"})
		return
	}
	r.presence.set(sender, in.Agents)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

func (r *Relay) presenceLimit() *limiter {
	r.presenceOnce.Do(func() { r.presenceLimiter = newLimiter(presenceRateMax, rateWindow) })
	return r.presenceLimiter
}

// LocalHandler serves this machine's agents and app: MCP at /mcp, and /local/presence for the app.
func (r *Relay) LocalHandler() http.Handler {
	mux := http.NewServeMux()
	mcpH := r.MCPHandler()
	mux.Handle("/mcp", mcpH)
	mux.Handle("/mcp/", mcpH)
	mux.HandleFunc("/local/presence", r.localPresence)
	return mux
}

// localPresence: POST sends this machine's agent statuses to every peer; GET returns what peers sent.
func (r *Relay) localPresence(w http.ResponseWriter, req *http.Request) {
	got, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
	if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(r.cfg.MCPToken)) != 1 {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing or bad token"})
		return
	}
	switch req.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"peers": r.presence.all()})
	case http.MethodPost:
		var in presenceIn
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 128<<10)).Decode(&in); err != nil || !validAgents(in.Agents) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "expected {agents: [{card, state, question?, since?}]}"})
			return
		}
		writeJSON(w, http.StatusOK, r.sharePresence(req.Context(), in))
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET or POST"})
	}
}

// sharePresence posts the list to every peer at once; an unreachable peer only shows in `failed`.
func (r *Relay) sharePresence(ctx context.Context, in presenceIn) map[string]any {
	payload, _ := json.Marshal(in)
	var mu sync.Mutex
	sent, failed := []string{}, map[string]string{}
	var wg sync.WaitGroup
	for _, name := range r.cfg.PeerNames() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(pctx, http.MethodPost, r.peerURL(name, "/v1/presence"), bytes.NewReader(payload))
			req.Header.Set("Content-Type", "application/json")
			resp, err := r.client.Do(req)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				failed[name] = err.Error()
			case resp.StatusCode != http.StatusAccepted:
				failed[name] = resp.Status
			default:
				sent = append(sent, name)
			}
			if resp != nil {
				resp.Body.Close()
			}
		}()
	}
	wg.Wait()
	return map[string]any{"sent": sent, "failed": failed}
}
