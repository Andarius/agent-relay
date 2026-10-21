package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// requireToken accepts the token as a bearer header or as the path /mcp/<token> (for clients that cannot set headers).
func requireToken(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			got, ok = strings.CutPrefix(r.URL.Path, "/mcp/")
		}
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing or bad token"})
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/mcp"
		next.ServeHTTP(w, r2)
	})
}

// loadOrCreateToken reads the token file, creating a random owner-only one when it does not exist.
func loadOrCreateToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		if tok := strings.TrimSpace(string(data)); len(tok) >= 32 {
			return tok, nil
		}
		return "", errors.New(path + " holds a token shorter than 32 chars")
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(buf)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	return tok, os.WriteFile(path, []byte(tok+"\n"), 0o600)
}
