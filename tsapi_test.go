package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tailscale/hujson"
)

const freshPolicy = `{
	// keep me
	"grants": [
		{"src": ["*"], "dst": ["*"], "ip": ["*"]},
	],
	"ssh": [{"action": "check", "src": ["autogroup:member"], "dst": ["autogroup:self"], "users": ["autogroup:nonroot"]}],
}`

const friendPolicy = `{
	"tagOwners": {"tag:agent-julien": ["autogroup:admin"], "tag:other": ["autogroup:admin"]},
	"grants": [
		{"src": ["*"], "dst": ["*"], "ip": ["*"]},
		{"src": ["tag:agent-julien"], "dst": ["tag:agent-julien"], "ip": ["tcp:80"]},
	],
}`

func parsePolicy(t *testing.T, b []byte) (map[string][]string, []grantRule) {
	t.Helper()
	v, err := hujson.Standardize(slices.Clone(b))
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		TagOwners map[string][]string `json:"tagOwners"`
		Grants    []grantRule         `json:"grants"`
	}
	if err := json.Unmarshal(v, &p); err != nil {
		t.Fatal(err)
	}
	return p.TagOwners, p.Grants
}

func TestPlanPolicy(t *testing.T) {
	agent := func(tags ...string) grantRule { return grantRule{Src: tags, Dst: tags, IP: []string{"tcp:80"}} }
	open := grantRule{Src: []string{"*"}, Dst: []string{"*"}, IP: []string{"*"}}
	cases := []struct {
		name      string
		in        string
		tags      []string
		closeOpen bool
		addTags   []string
		grants    []grantRule
	}{
		{"fresh tailnet closed", freshPolicy, []string{"tag:agent-julien"}, true,
			[]string{"tag:agent-julien"}, []grantRule{agent("tag:agent-julien")}},
		{"shared tailnet keeps allow-all", freshPolicy, []string{"tag:agent-julien"}, false,
			[]string{"tag:agent-julien"}, []grantRule{open, agent("tag:agent-julien")}},
		{"friend joins existing grant", friendPolicy, []string{"tag:agent-alice"}, true,
			[]string{"tag:agent-alice"}, []grantRule{agent("tag:agent-alice", "tag:agent-julien")}},
		{"no policy sections", `{}`, []string{"tag:agent-julien"}, true,
			[]string{"tag:agent-julien"}, []grantRule{agent("tag:agent-julien")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan, err := planPolicy([]byte(c.in), c.tags, c.closeOpen)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(plan.AddTags, c.addTags) {
				t.Errorf("AddTags = %v, want %v", plan.AddTags, c.addTags)
			}
			owners, grants := parsePolicy(t, plan.Out)
			for _, tag := range c.tags {
				if _, ok := owners[tag]; !ok {
					t.Errorf("tagOwners misses %s", tag)
				}
			}
			if len(grants) != len(c.grants) {
				t.Fatalf("grants = %+v, want %+v", grants, c.grants)
			}
			for i := range grants {
				if !slices.Equal(grants[i].Src, c.grants[i].Src) || !slices.Equal(grants[i].Dst, c.grants[i].Dst) ||
					!slices.Equal(grants[i].IP, c.grants[i].IP) {
					t.Errorf("grant %d = %+v, want %+v", i, grants[i], c.grants[i])
				}
			}
			if strings.Contains(c.in, "keep me") && !strings.Contains(string(plan.Out), "keep me") {
				t.Error("comment lost")
			}
			again, err := planPolicy(plan.Out, c.tags, c.closeOpen)
			if err != nil || again.Changed() {
				t.Errorf("second run should be a no-op: %+v %v", again, err)
			}
		})
	}
}

func TestCreateKeyRequest(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tskey-api-x" || r.URL.Path != "/tailnet/-/keys" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"key":"tskey-auth-y"}`))
	}))
	defer srv.Close()
	api := &TSAPI{Base: srv.URL, Token: "tskey-api-x", HTTP: srv.Client()}
	key, err := api.CreateKey(t.Context(), []string{"tag:agent-alice"}, "agent-relay alice", time.Hour)
	if err != nil || key != "tskey-auth-y" {
		t.Fatalf("key=%q err=%v", key, err)
	}
	create := got["capabilities"].(map[string]any)["devices"].(map[string]any)["create"].(map[string]any)
	if create["reusable"] != false || create["preauthorized"] != true || create["ephemeral"] != false {
		t.Errorf("key must be single-use, pre-approved, persistent: %v", create)
	}
	api.Token = "wrong"
	if _, err := api.CreateKey(t.Context(), nil, "x", time.Hour); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Errorf("want token rejected error, got %v", err)
	}
}
