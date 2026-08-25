package process

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRunExitAndBoundedStreams(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture uses sh")
	}
	command := `head -c 1100000 /dev/zero | tr '\000' o; head -c 1100000 /dev/zero | tr '\000' e >&2; exit 7`
	stdout, stderr, exitCode, err := Run(context.Background(), t.TempDir(), "sh", []string{"-c", command})
	if len(stdout) != OutputLimit+len(outputTruncatedMarker) || len(stderr) != OutputLimit+len(outputTruncatedMarker) {
		t.Fatalf("output lengths = (%d, %d), want (%d, %d)", len(stdout), len(stderr), OutputLimit+len(outputTruncatedMarker), OutputLimit+len(outputTruncatedMarker))
	}
	if !strings.HasSuffix(stdout, outputTruncatedMarker) || !strings.HasSuffix(stderr, outputTruncatedMarker) {
		t.Fatalf("truncation markers missing from bounded output")
	}
	if exitCode != 7 {
		t.Fatalf("exit code = %d, want 7", exitCode)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("error = %T %v, want *exec.ExitError", err, err)
	}
}

func TestRunCancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture uses sh")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, _, exitCode, err := Run(ctx, "", "sh", []string{"-c", "sleep 30 & wait"})
	if !errors.Is(err, context.DeadlineExceeded) || exitCode != -1 {
		t.Fatalf("Run cancellation = exit %d, err %v", exitCode, err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("cancellation took %s", elapsed)
	}
}

func TestMinimalEnvironmentAllowlistAndOverrides(t *testing.T) {
	t.Setenv("PATH", "/test/bin")
	t.Setenv("HOME", "/test/home")
	t.Setenv("OPENAI_API_KEY", "secret")
	t.Setenv("GITHUB_TOKEN", "secret")
	t.Setenv("CI_JOB_TOKEN", "secret")
	t.Setenv("NOTCH_TEST_SECRET", "secret")

	environment := MinimalEnvironment(map[string]string{"HOME": "/override", "EXPLICIT_TOKEN": "configured"})
	joined := "\n" + strings.Join(environment, "\n") + "\n"
	for _, want := range []string{"PATH=/test/bin", "HOME=/override", "EXPLICIT_TOKEN=configured"} {
		if !strings.Contains(joined, "\n"+want+"\n") {
			t.Fatalf("environment missing %q: %q", want, environment)
		}
	}
	for _, unwanted := range []string{"OPENAI_API_KEY=", "GITHUB_TOKEN=", "CI_JOB_TOKEN=", "NOTCH_TEST_SECRET="} {
		if strings.Contains(joined, unwanted) {
			t.Fatalf("environment leaked %q: %q", unwanted, environment)
		}
	}
	for _, entry := range environment {
		if strings.HasPrefix(entry, "HOME=") && entry != "HOME=/override" {
			t.Fatalf("override left inherited duplicate: %q", environment)
		}
	}
}
