// Command versionprobe answers one MCP initialize as the foreman binary
// would, and prints serverInfo.version: TestLinkerSetsServerInfoVersion
// builds it with build.sh's -ldflags -X and checks the version arrives.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"

	"github.com/stevegeek/agent-factory/internal/guard"
	"github.com/stevegeek/agent-factory/internal/mcp"
	"github.com/stevegeek/agent-factory/internal/tokens"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	dir, err := os.MkdirTemp("", "versionprobe")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	store := filepath.Join(dir, "api-tokens.json")
	secret := "fct_versionprobe"
	sum := sha256.Sum256([]byte(secret))
	data, _ := json.Marshal(map[string]any{"version": 1, "tokens": []any{map[string]any{
		"name": "probe-agent", "sha256": hex.EncodeToString(sum[:]), "projects": []string{"p"}, "can": []string{"read"}}}})
	if err := os.WriteFile(store, data, 0o600); err != nil {
		return err
	}
	srv := mcp.New(mcp.Options{Guard: guard.New(guard.Options{Tokens: &tokens.Store{Path: store}})})
	r := httptest.NewRequest("POST", mcp.Path,
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`))
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+secret)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	var out struct {
		Result struct {
			ServerInfo struct{ Version string } `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		return fmt.Errorf("%d %s: %w", w.Code, w.Body.String(), err)
	}
	fmt.Println(out.Result.ServerInfo.Version)
	return nil
}
