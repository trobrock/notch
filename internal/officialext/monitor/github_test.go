package monitor

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestGithubMonitorBuildsSafeArgv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("helper script uses sh")
	}
	dir := t.TempDir()
	output := filepath.Join(dir, "args")
	script := filepath.Join(dir, "gh")
	body := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$GH_ARGS\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GH_ARGS", output)
	r, _ := setup(t)
	tool, _ := r.Tool("monitor_github_pr_checks")
	result, err := tool.Execute(context.Background(), []byte(`{"pr":"42","repo":"owner/repo","requiredOnly":true,"failFast":true,"intervalSeconds":15}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Details["id"] != "mon-1" {
		t.Fatalf("result=%#v", result)
	}
	waitFor(t, func() bool { _, err := os.Stat(output); return err == nil })
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	want := "pr\nchecks\n42\n--watch\n--interval\n15\n--required\n--fail-fast\n--repo\nowner/repo\n"
	if string(data) != want {
		t.Fatalf("args=%q want=%q", data, want)
	}
}

func TestGithubMonitorValidatesInterval(t *testing.T) {
	r, _ := setup(t)
	tool, _ := r.Tool("monitor_github_pr_checks")
	if _, err := tool.Execute(context.Background(), []byte(`{"intervalSeconds":5}`), nil); err == nil {
		t.Fatal("invalid interval succeeded")
	}
}
