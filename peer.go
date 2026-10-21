package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"tailscale.com/client/tailscale/apitype"
)

type whoIser interface {
	WhoIs(ctx context.Context, remoteAddr string) (*apitype.WhoIsResponse, error)
}

// Relay holds everything both the peer API and the MCP tools need.
type Relay struct {
	cfg     *Config
	store   *Store
	client  *http.Client
	whois   whoIser
	limiter *limiter
}

type peerMessage struct {
	ID   string `json:"id"`
	Body string `json:"body"`
}

// PeerHandler serves the tailnet-facing API; the sender is derived from Tailscale identity, never from the payload.
func (r *Relay) PeerHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"name": r.cfg.Name})
	})
	mux.HandleFunc("POST /v1/messages", r.receive)
	return mux
}

func (r *Relay) receive(w http.ResponseWriter, req *http.Request) {
	who, err := r.whois.WhoIs(req.Context(), req.RemoteAddr)
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "unknown caller"})
		return
	}
	sender, login, ok := r.authorize(who)
	if !ok {
		slog.Warn("rejected message", "node", nodeName(who), "login", login)
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}
	var msg peerMessage
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, maxBody+1024)).Decode(&msg); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "expected {id, body}"})
		return
	}
	if _, err := uuid.Parse(msg.ID); err != nil || msg.Body == "" || len(msg.Body) > maxBody {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad id or body"})
		return
	}
	if !r.limiter.allow(sender) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limited"})
		return
	}
	inserted, err := r.store.Insert(req.Context(), msg.ID, "in", sender, msg.Body, login)
	if err != nil {
		slog.Error("store", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "store failed"})
		return
	}
	if inserted {
		slog.Info("in", "id", msg.ID, "from", sender, "chars", len(msg.Body))
		r.ringDoorbell(msg.ID, sender)
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"id": msg.ID, "status": "accepted"})
}

// authorize maps the calling node to a configured peer and checks the identity that peer must present.
func (r *Relay) authorize(who *apitype.WhoIsResponse) (sender, login string, ok bool) {
	if who.UserProfile != nil {
		login = who.UserProfile.LoginName
	}
	node := nodeName(who)
	for name, p := range r.cfg.Peers {
		if !strings.EqualFold(p.Host, node) {
			continue
		}
		if p.Login != "" && p.Login == login {
			return name, login, true
		}
		if p.Tag != "" && who.Node != nil && slices.Contains(who.Node.Tags, p.Tag) {
			return name, login, true
		}
	}
	return "", login, false
}

// nodeName returns the caller's MagicDNS short name (shared-in nodes report an FQDN).
func nodeName(who *apitype.WhoIsResponse) string {
	if who == nil || who.Node == nil {
		return ""
	}
	name, _, _ := strings.Cut(who.Node.ComputedName, ".")
	return name
}

// ringDoorbell drops a body-less marker in the agent workspace so an in-sandbox watcher can wake the agent.
func (r *Relay) ringDoorbell(id, sender string) {
	if r.cfg.InboxDir() == "" {
		return
	}
	if err := os.MkdirAll(r.cfg.InboxDir(), 0o755); err != nil {
		slog.Error("doorbell", "err", err)
		return
	}
	data, _ := json.Marshal(map[string]string{"id": id, "from": sender})
	if err := os.WriteFile(filepath.Join(r.cfg.InboxDir(), id+".json"), data, 0o644); err != nil {
		slog.Error("doorbell", "err", err)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	hits   map[string][]time.Time
}

func newLimiter(max int, window time.Duration) *limiter {
	return &limiter{max: max, window: window, hits: map[string][]time.Time{}}
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := time.Now().Add(-l.window)
	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.max {
		l.hits[key] = kept
		return false
	}
	l.hits[key] = append(kept, time.Now())
	return true
}
