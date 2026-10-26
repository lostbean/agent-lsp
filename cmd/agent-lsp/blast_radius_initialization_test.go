package main

import (
	"context"
	"encoding/json"
	"github.com/blackwell-systems/agent-lsp/internal/config"
	"github.com/blackwell-systems/agent-lsp/internal/lsp"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestImpactLSPProcess runs a real framed-stdio protocol peer in a child test
// process. It has no external language-server installation requirement.
func TestImpactLSPProcess(t *testing.T) {
	if os.Args[len(os.Args)-1] != "impact-lsp-peer" {
		return
	}
	reader := lsp.NewFrameReader(os.Stdin)
	for {
		raw, err := reader.ReadMessage()
		if err != nil {
			os.Exit(0)
		}
		var message struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(raw, &message) != nil {
			os.Exit(1)
		}
		if message.Method == "exit" {
			os.Exit(0)
		}
		if len(message.ID) == 0 {
			continue
		}
		result := any([]any{})
		if message.Method == "initialize" {
			result = map[string]any{"capabilities": map[string]any{"documentSymbolProvider": true, "referencesProvider": true}}
		} else if message.Method == "shutdown" {
			result = nil
		}
		payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": message.ID, "result": result})
		if _, err := os.Stdout.Write(lsp.EncodeMessage(payload)); err != nil {
			os.Exit(1)
		}
	}
}

func TestBlastRadius_InitializesSelectedServer(t *testing.T) {
	t.Setenv("AGENT_LSP_DISABLE_WATCHER", "1")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example\n"), 0600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "empty.go")
	if err := os.WriteFile(file, []byte("package example\n"), 0600); err != nil {
		t.Fatal(err)
	}
	rustFile := filepath.Join(root, "script.rs")
	if err := os.WriteFile(rustFile, []byte("fn main() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	resolver := lsp.NewMultiServerManager([]config.ServerEntry{
		{LanguageID: "rust", Extensions: []string{"rs"}, Command: []string{filepath.Join(root, "unavailable-rust-server")}},
		{LanguageID: "go", Extensions: []string{"go"}, Command: []string{executable, "-test.run=^TestImpactLSPProcess$", "--", "impact-lsp-peer"}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	defer resolver.Shutdown(context.Background())
	state := &clientState{}
	var initMu sync.Mutex
	server := mcp.NewServer(&mcp.Implementation{Name: "impact-test", Version: "1"}, nil)
	deps := toolDeps{server: server, cs: state, resolver: resolver,
		clientForFileWithAutoInit: func(path string) *lsp.LSPClient {
			if c := clientForFile(resolver, state, path); c != nil && c.IsInitialized() {
				return c
			}
			return autoInitClient(ctx, resolver, state, &initMu, path)
		},
	}
	registerAnalysisTools(deps)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	for i := 0; i < 2; i++ {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "blast_radius", Arguments: map[string]any{"changed_files": []string{file}}})
		if err != nil {
			t.Fatal(err)
		}
		if result.IsError {
			t.Fatalf("cold impact query failed: %v", result.Content)
		}
		text := ""
		for _, content := range result.Content {
			if item, ok := content.(*mcp.TextContent); ok {
				text += item.Text
			}
		}
		if !strings.Contains(text, "changed_symbols") {
			t.Fatalf("missing impact result: %s", text)
		}
	}
	// A warm Go connection must not stand in for the configured, unstarted
	// Rust server. Its unavailable command must be reported as a failure.
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "blast_radius", Arguments: map[string]any{"changed_files": []string{rustFile}}})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatalf("Rust query used the warm Go server: %v", result.Content)
	}
	// Failing to start Rust must leave the Go server usable.
	result, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "blast_radius", Arguments: map[string]any{"changed_files": []string{file}}})
	if err != nil || result.IsError {
		t.Fatalf("Go query failed after Rust startup failure: %v %v", err, result)
	}

}
