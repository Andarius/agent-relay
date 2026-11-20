package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type PeerStatus struct {
	Name      string `json:"name"`
	Reachable bool   `json:"reachable"`
	Error     string `json:"error,omitempty"`
}

type ListPeersOut struct {
	Peers []PeerStatus `json:"peers"`
}

type SendIn struct {
	To   string `json:"to" jsonschema:"peer name from list_peers"`
	Body string `json:"body" jsonschema:"message text"`
	Card string `json:"card,omitempty" jsonschema:"id of the Trame card the message is about, if any"`
}

type SendOut struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type InboxIn struct {
	IncludeRead bool   `json:"include_read,omitempty" jsonschema:"also return messages already read"`
	Limit       int    `json:"limit,omitempty" jsonschema:"max messages, 1-100, default 20"`
	Card        string `json:"card,omitempty" jsonschema:"only messages about this Trame card id"`
}

type InboxOut struct {
	Note     string    `json:"note"`
	Messages []Message `json:"messages"`
}

// MCPHandler serves the host-local MCP endpoint the agent (or the sbx gateway) connects to.
func (r *Relay) MCPHandler() http.Handler {
	srv := mcp.NewServer(&mcp.Implementation{Name: "agent-relay", Version: "0.2.0"}, &mcp.ServerOptions{
		Instructions: fmt.Sprintf("You are %q. Talk to agents on other machines. %s", r.cfg.Name, untrustedMsg),
	})
	mcp.AddTool(srv, &mcp.Tool{Name: "list_peers", Description: "List remote agents you can message and whether their relay answers."}, r.listPeers)
	mcp.AddTool(srv, &mcp.Tool{Name: "send_message", Description: "Send a text message to the agent on another machine."}, r.sendMessage)
	mcp.AddTool(srv, &mcp.Tool{Name: "read_inbox", Description: "Return received messages, oldest first, and mark them read."}, r.readInbox)
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	return requireToken(r.cfg.MCPToken, h)
}

func (r *Relay) peerURL(name, path string) string {
	return fmt.Sprintf("http://%s:%d%s", r.cfg.Peers[name].Host, peerPort, path)
}

func (r *Relay) listPeers(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, ListPeersOut, error) {
	out := ListPeersOut{Peers: make([]PeerStatus, 0, len(r.cfg.Peers))}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for name := range r.cfg.Peers {
		wg.Go(func() {
			st := PeerStatus{Name: name}
			pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(pctx, http.MethodGet, r.peerURL(name, "/v1/health"), nil)
			if resp, err := r.client.Do(req); err != nil {
				st.Error = err.Error()
			} else {
				resp.Body.Close()
				st.Reachable = resp.StatusCode == http.StatusOK
			}
			mu.Lock()
			out.Peers = append(out.Peers, st)
			mu.Unlock()
		})
	}
	wg.Wait()
	sort.Slice(out.Peers, func(i, j int) bool { return out.Peers[i].Name < out.Peers[j].Name })
	return nil, out, nil
}

func (r *Relay) sendMessage(ctx context.Context, _ *mcp.CallToolRequest, in SendIn) (*mcp.CallToolResult, SendOut, error) {
	if _, ok := r.cfg.Peers[in.To]; !ok {
		return nil, SendOut{}, fmt.Errorf("unknown peer %q; known: %s", in.To, strings.Join(r.cfg.PeerNames(), ", "))
	}
	if strings.TrimSpace(in.Body) == "" || len(in.Body) > maxBody {
		return nil, SendOut{}, fmt.Errorf("body must be 1..%d chars", maxBody)
	}
	if !validCard(in.Card) {
		return nil, SendOut{}, fmt.Errorf("card must be a card id (UUID)")
	}
	id := uuid.NewString()
	payload, _ := json.Marshal(peerMessage{ID: id, Body: in.Body, Card: in.Card})
	pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(pctx, http.MethodPost, r.peerURL(in.To, "/v1/messages"), bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, SendOut{}, fmt.Errorf("could not reach %s: %w", in.To, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return nil, SendOut{}, fmt.Errorf("%s refused the message: %d %s", in.To, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if _, err := r.store.Insert(ctx, id, "out", in.To, in.Body, "", in.Card); err != nil {
		slog.Error("store outgoing", "err", err)
	}
	slog.Info("out", "id", id, "to", in.To, "chars", len(in.Body))
	return nil, SendOut{ID: id, Status: "delivered"}, nil
}

func (r *Relay) readInbox(ctx context.Context, _ *mcp.CallToolRequest, in InboxIn) (*mcp.CallToolResult, InboxOut, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}
	msgs, err := r.store.TakeInbox(ctx, in.IncludeRead, min(limit, 100), in.Card)
	if err != nil {
		return nil, InboxOut{}, err
	}
	return nil, InboxOut{Note: untrustedMsg, Messages: msgs}, nil
}
