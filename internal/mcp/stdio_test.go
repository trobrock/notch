package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	sharedprocess "github.com/trobrock/notch/internal/process"
)

func TestLockedBufferBoundsStderr(t *testing.T) {
	var buffer lockedBuffer
	payload := strings.Repeat("x", sharedprocess.OutputLimit+100)
	if written, err := buffer.Write([]byte(payload)); err != nil || written != len(payload) {
		t.Fatalf("Write = %d, %v", written, err)
	}
	got := buffer.String()
	if !strings.HasSuffix(got, "\n[stderr truncated]") || len(got) > sharedprocess.OutputLimit+32 {
		t.Fatalf("bounded stderr length/suffix = %d, %q", len(got), got[len(got)-min(len(got), 32):])
	}
}

func TestStdioEnvironmentIsMinimalAndConfigOverrides(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "provider-secret")
	t.Setenv("GITHUB_TOKEN", "ci-secret")
	t.Setenv("CI_JOB_TOKEN", "ci-secret")
	t.Setenv("NOTCH_PRIVATE_VALUE", "private")
	t.Setenv("HOME", "/inherited-home")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	client, err := newStdioClient(context.Background(), ServerConfig{
		Command: executable, Args: []string{"-test.run=^TestMCPStdioEnvironmentHelper$"},
		Env: map[string]string{"HOME": "/configured-home", "EXPLICIT_TOKEN": "configured"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var response struct {
		Environment []string `json:"environment"`
	}
	if err := client.call(ctx, "environment", map[string]any{}, &response); err != nil {
		t.Fatal(err)
	}
	joined := "\n" + strings.Join(response.Environment, "\n") + "\n"
	for _, wanted := range []string{"HOME=/configured-home", "EXPLICIT_TOKEN=configured"} {
		if !strings.Contains(joined, "\n"+wanted+"\n") {
			t.Fatalf("child environment missing %q: %q", wanted, response.Environment)
		}
	}
	for _, secret := range []string{"OPENAI_API_KEY=", "GITHUB_TOKEN=", "CI_JOB_TOKEN=", "NOTCH_PRIVATE_VALUE="} {
		if strings.Contains(joined, secret) {
			t.Fatalf("child environment leaked %q: %q", secret, response.Environment)
		}
	}
	if strings.Contains(joined, "HOME=/inherited-home") {
		t.Fatalf("configured HOME did not override inherited value: %q", response.Environment)
	}
}

func TestMCPStdioEnvironmentHelper(t *testing.T) {
	helper := false
	for _, arg := range os.Args[1:] {
		if arg == "-test.run=^TestMCPStdioEnvironmentHelper$" {
			helper = true
			break
		}
	}
	if !helper {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var request struct {
			ID json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			os.Exit(2)
		}
		if err := encoder.Encode(map[string]any{
			"jsonrpc": "2.0", "id": request.ID,
			"result": map[string]any{"environment": os.Environ()},
		}); err != nil {
			os.Exit(2)
		}
	}
}
