package main

import (
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeServerCert(t *testing.T, server *httptest.Server) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "server.crt")
	certificate := server.Certificate()
	data := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMCPImportPiHelper(t *testing.T) {
	if os.Getenv("GO_WANT_MCP_IMPORT_HELPER") != "1" {
		return
	}
	if err := runMCP([]string{"import-pi", os.Getenv("PI_MCP_CONFIG")}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestImportPiMCPFromKeyring(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	secretTool := filepath.Join(bin, "secret-tool")
	payload := map[string]any{
		"tokens":     map[string]any{"accessToken": "access", "refreshToken": "refresh", "expiresAt": int64(2_000_000_000), "scope": "read"},
		"clientInfo": map[string]any{"clientId": "client", "tokenEndpointAuthMethod": "none"},
	}
	encoded, _ := json.Marshal(payload)
	script := "#!/bin/sh\nprintf '%s\\n' '" + string(encoded) + "'\n"
	if err := os.WriteFile(secretTool, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	notchHome := filepath.Join(home, ".notch")
	if err := os.MkdirAll(notchHome, 0o700); err != nil {
		t.Fatal(err)
	}
	piConfig := filepath.Join(home, "pi-mcp.json")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := "https://" + r.Host
		switch r.URL.Path {
		case "/.well-known/oauth-protected-resource/mcp":
			_ = json.NewEncoder(w).Encode(map[string]any{"resource": origin + "/mcp", "authorization_servers": []string{origin}})
		case "/.well-known/oauth-authorization-server":
			_ = json.NewEncoder(w).Encode(map[string]any{"issuer": origin, "token_endpoint": origin + "/token"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	mcpJSON := `{"mcpServers":{"remote":{"url":"` + server.URL + `/mcp","auth":"oauth"}}}`
	if err := os.WriteFile(piConfig, []byte(mcpJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(notchHome, "mcp.json"), []byte(mcpJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestMCPImportPiHelper$")
	command.Env = append(os.Environ(),
		"GO_WANT_MCP_IMPORT_HELPER=1", "HOME="+home, "NOTCH_HOME="+notchHome,
		"PI_MCP_CONFIG="+piConfig, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	command.Env = append(command.Env, "SSL_CERT_FILE="+writeServerCert(t, server))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("import failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "imported 1") {
		t.Fatalf("output = %q", output)
	}
	data, err := os.ReadFile(filepath.Join(notchHome, "mcp-auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "evil") || !strings.Contains(string(data), `"access_token": "access"`) {
		t.Fatalf("credential store = %s", data)
	}
}
