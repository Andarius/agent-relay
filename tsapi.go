package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/tailscale/hujson"
)

const (
	tsAPIDefault   = "https://api.tailscale.com/api/v2"
	agentTagPrefix = "tag:agent-"
)

// TSAPI is the slice of the Tailscale API that running an agent network needs.
type TSAPI struct {
	Base  string
	Token string
	HTTP  *http.Client
}

func NewTSAPI(token string) *TSAPI {
	base := os.Getenv("TS_API_URL")
	if base == "" {
		base = tsAPIDefault
	}
	return &TSAPI{Base: strings.TrimRight(base, "/"), Token: token, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

type APIError struct {
	Status int
	Msg    string
}

func (e *APIError) Error() string {
	if e.Status == http.StatusUnauthorized {
		return "Tailscale rejected the API token (expired or mistyped?)"
	}
	return fmt.Sprintf("Tailscale API %d: %s", e.Status, e.Msg)
}

func (a *TSAPI) do(ctx context.Context, method, path string, body []byte, hdr map[string]string) (http.Header, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, a.Base+path, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.Token)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode >= 300 {
		var m struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &m) != nil || m.Message == "" {
			m.Message = strings.TrimSpace(string(data))
		}
		return nil, nil, &APIError{Status: resp.StatusCode, Msg: m.Message}
	}
	return resp.Header, data, nil
}

// Policy returns the tailnet policy file and its ETag.
func (a *TSAPI) Policy(ctx context.Context) ([]byte, string, error) {
	h, data, err := a.do(ctx, http.MethodGet, "/tailnet/-/acl", nil, map[string]string{"Accept": "application/hujson"})
	if err != nil {
		return nil, "", err
	}
	return data, h.Get("ETag"), nil
}

// SetPolicy replaces the policy file unless someone changed it since etag was read.
func (a *TSAPI) SetPolicy(ctx context.Context, body []byte, etag string) error {
	hdr := map[string]string{"Content-Type": "application/hujson"}
	if etag != "" {
		hdr["If-Match"] = etag
	}
	_, _, err := a.do(ctx, http.MethodPost, "/tailnet/-/acl", body, hdr)
	return err
}

var keyDescRe = regexp.MustCompile(`[^a-zA-Z0-9 -]`)

// CreateKey mints a single-use, pre-approved auth key that joins nodes with tags.
func (a *TSAPI) CreateKey(ctx context.Context, tags []string, desc string, ttl time.Duration) (string, error) {
	desc = keyDescRe.ReplaceAllString(desc, "-")
	if len(desc) > 50 {
		desc = desc[:50]
	}
	body, _ := json.Marshal(map[string]any{
		"capabilities": map[string]any{"devices": map[string]any{"create": map[string]any{
			"reusable": false, "ephemeral": false, "preauthorized": true, "tags": tags,
		}}},
		"expirySeconds": int(ttl.Seconds()),
		"description":   desc,
	})
	_, data, err := a.do(ctx, http.MethodPost, "/tailnet/-/keys", body, map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return "", err
	}
	var k struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(data, &k); err != nil || k.Key == "" {
		return "", errors.New("Tailscale API: no key in the response")
	}
	return k.Key, nil
}

type Device struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Hostname string   `json:"hostname"`
	Tags     []string `json:"tags"`
}

// ShortName is the device's MagicDNS label, which is what peers match on.
func (d Device) ShortName() string {
	name, _, _ := strings.Cut(d.Name, ".")
	return name
}

func (a *TSAPI) Devices(ctx context.Context) ([]Device, error) {
	_, data, err := a.do(ctx, http.MethodGet, "/tailnet/-/devices", nil, nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Devices []Device `json:"devices"`
	}
	return out.Devices, json.Unmarshal(data, &out)
}

func (a *TSAPI) DeleteDevice(ctx context.Context, id string) error {
	_, _, err := a.do(ctx, http.MethodDelete, "/device/"+id, nil, nil)
	return err
}

// Policy merge

// PolicyPlan is the change planPolicy makes; Out is the full new policy.
type PolicyPlan struct {
	AddTags   []string
	GrantTags []string
	OpenRules int
	Closed    bool
	Out       []byte
}

func (p PolicyPlan) Changed() bool { return len(p.AddTags) > 0 || p.Closed || p.GrantTags != nil }

type grantRule struct {
	Src []string `json:"src"`
	Dst []string `json:"dst"`
	IP  []string `json:"ip"`
}

type aclRule struct {
	Action string   `json:"action"`
	Src    []string `json:"src"`
}

// isAgentGrant spots the one grant agent-relay owns: agent tags to agent tags on tcp:80.
func isAgentGrant(g grantRule) bool {
	if !slices.Equal(g.IP, []string{fmt.Sprintf("tcp:%d", peerPort)}) || len(g.Src) == 0 {
		return false
	}
	for _, t := range append(slices.Clone(g.Src), g.Dst...) {
		if !strings.HasPrefix(t, agentTagPrefix) {
			return false
		}
	}
	return true
}

func pointerEsc(s string) string {
	return strings.NewReplacer("~", "~0", "/", "~1").Replace(s)
}

// planPolicy adds tags to the policy and fences all agent tags to each other's relay port, keeping comments.
// With closeOpen it also drops allow-all rules, which only belongs on a tailnet dedicated to agents.
func planPolicy(in []byte, tags []string, closeOpen bool) (PolicyPlan, error) {
	var plan PolicyPlan
	v, err := hujson.Parse(in)
	if err != nil {
		return plan, fmt.Errorf("policy file: %w", err)
	}
	std := v.Clone()
	std.Standardize()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(std.Pack(), &top); err != nil {
		return plan, fmt.Errorf("policy file: %w", err)
	}
	// Tailscale matches top-level keys case-insensitively; patches need the exact spelling.
	key := func(name string) string {
		for k := range top {
			if strings.EqualFold(k, name) {
				return k
			}
		}
		return ""
	}
	var owners map[string][]string
	var grants []grantRule
	var acls []aclRule
	for name, dst := range map[string]any{"tagOwners": &owners, "grants": &grants, "acls": &acls} {
		if k := key(name); k != "" {
			if err := json.Unmarshal(top[k], dst); err != nil {
				return plan, fmt.Errorf("policy %s: %w", name, err)
			}
		}
	}

	var ops []map[string]any
	ownersKey := key("tagOwners")
	if ownersKey == "" {
		ownersKey = "tagOwners"
		ops = append(ops, map[string]any{"op": "add", "path": "/tagOwners", "value": map[string]any{}})
	}
	for _, t := range tags {
		if _, ok := owners[t]; !ok {
			plan.AddTags = append(plan.AddTags, t)
			ops = append(ops, map[string]any{"op": "add", "path": "/" + pointerEsc(ownersKey) + "/" + pointerEsc(t),
				"value": []string{"autogroup:admin"}})
		}
	}

	all := slices.Clone(tags)
	for t := range owners {
		if strings.HasPrefix(t, agentTagPrefix) {
			all = append(all, t)
		}
	}
	agentIdx := -1
	for i, g := range grants {
		if isAgentGrant(g) {
			agentIdx = i
			all = append(append(all, g.Src...), g.Dst...)
			break
		}
	}
	sort.Strings(all)
	all = slices.Compact(all)

	var openGrants, openACLs []int
	for i, g := range grants {
		if slices.Contains(g.Src, "*") {
			openGrants = append(openGrants, i)
		}
	}
	for i, r := range acls {
		if r.Action == "accept" && slices.Contains(r.Src, "*") {
			openACLs = append(openACLs, i)
		}
	}
	plan.OpenRules = len(openGrants) + len(openACLs)
	grantsKey := key("grants")
	if closeOpen && plan.OpenRules > 0 {
		plan.Closed = true
		for _, i := range slices.Backward(openGrants) {
			ops = append(ops, map[string]any{"op": "remove", "path": fmt.Sprintf("/%s/%d", pointerEsc(grantsKey), i)})
			if i < agentIdx {
				agentIdx--
			}
		}
		for _, i := range slices.Backward(openACLs) {
			ops = append(ops, map[string]any{"op": "remove", "path": fmt.Sprintf("/%s/%d", pointerEsc(key("acls")), i)})
		}
	}

	want := grantRule{Src: all, Dst: all, IP: []string{fmt.Sprintf("tcp:%d", peerPort)}}
	switch {
	case grantsKey == "":
		ops = append(ops, map[string]any{"op": "add", "path": "/grants", "value": []grantRule{want}})
		plan.GrantTags = all
	case agentIdx < 0:
		ops = append(ops, map[string]any{"op": "add", "path": "/" + pointerEsc(grantsKey) + "/-", "value": want})
		plan.GrantTags = all
	default:
		cur := grants[slices.IndexFunc(grants, isAgentGrant)]
		if !slices.Equal(cur.Src, all) || !slices.Equal(cur.Dst, all) {
			ops = append(ops, map[string]any{"op": "replace", "path": fmt.Sprintf("/%s/%d", pointerEsc(grantsKey), agentIdx), "value": want})
			plan.GrantTags = all
		}
	}
	if len(ops) == 0 {
		plan.Out = in
		return plan, nil
	}
	patch, _ := json.Marshal(ops)
	if err := v.Patch(patch); err != nil {
		return plan, fmt.Errorf("policy patch: %w", err)
	}
	v.Format()
	plan.Out = v.Pack()
	return plan, nil
}

// EnsureTags merges tags into the tailnet policy; closeOpen also removes allow-all rules.
func (a *TSAPI) EnsureTags(ctx context.Context, tags []string, closeOpen bool) (PolicyPlan, error) {
	cur, etag, err := a.Policy(ctx)
	if err != nil {
		return PolicyPlan{}, err
	}
	plan, err := planPolicy(cur, tags, closeOpen)
	if err != nil || !plan.Changed() {
		return plan, err
	}
	return plan, a.SetPolicy(ctx, plan.Out, etag)
}

// API token storage

func apiTokenPath() (string, error) {
	p, err := configPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(p), "api-token"), nil
}

// loadAPIToken prefers $TS_API_KEY, then the token saved by init.
func loadAPIToken() string {
	if t := os.Getenv("TS_API_KEY"); t != "" {
		return t
	}
	p, err := apiTokenPath()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func saveAPIToken(tok string) error {
	p, err := apiTokenPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(tok+"\n"), 0o600)
}
