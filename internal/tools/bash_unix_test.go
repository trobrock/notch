//go:build !windows

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestBashCancellationAfterShellExits(t *testing.T) {
	tool := NewBash(t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	started := time.Now()
	_, err := tool.Execute(ctx, json.RawMessage(`{"command":"sleep 30 &"}`), nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("bash cancellation error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("bash cancellation took %s after the shell exited", elapsed)
	}
}
