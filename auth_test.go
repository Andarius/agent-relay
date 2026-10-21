package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestRequireToken(t *testing.T) {
	const tok = "0123456789abcdef0123456789abcdef"
	var gotPath string
	h := requireToken(tok, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	tests := []struct {
		name, path, header string
		want               int
	}{
		{"bearer header", "/mcp", "Bearer " + tok, http.StatusOK},
		{"token in path", "/mcp/" + tok, "", http.StatusOK},
		{"no token", "/mcp", "", http.StatusUnauthorized},
		{"wrong bearer", "/mcp", "Bearer nope", http.StatusUnauthorized},
		{"wrong path token", "/mcp/nope", "", http.StatusUnauthorized},
		{"token prefix only", "/mcp/" + tok[:10], "", http.StatusUnauthorized},
		{"non-bearer scheme", "/mcp", "Basic " + tok, http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotPath = ""
			req := httptest.NewRequest(http.MethodPost, tt.path, nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Fatalf("code = %d, want %d", rec.Code, tt.want)
			}
			if tt.want == http.StatusOK && gotPath != "/mcp" {
				t.Fatalf("inner path = %q, want /mcp", gotPath)
			}
		})
	}
}

func TestLoadOrCreateToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "mcp-token")
	first, err := loadOrCreateToken(path)
	if err != nil || len(first) != 64 {
		t.Fatalf("create: %q, %v", first, err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
	if again, err := loadOrCreateToken(path); err != nil || again != first {
		t.Fatalf("reload changed the token: %q, %v", again, err)
	}
	short := filepath.Join(dir, "short")
	_ = os.WriteFile(short, []byte("abc\n"), 0o600)
	if _, err := loadOrCreateToken(short); err == nil {
		t.Fatal("short token accepted")
	}
}
