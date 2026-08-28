// Package process provides shared process execution and child-environment helpers.
package process

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"
)

const (
	// OutputLimit is the maximum number of bytes retained from each output
	// stream of a host command. The process is still fully drained after the
	// limit is reached so a noisy child cannot block on a full pipe.
	OutputLimit = 1 << 20

	outputTruncatedMarker = "\n[output truncated]\n"
	processWaitDelay      = time.Second
)

// configureCommand applies platform cancellation and bounds how long inherited
// output pipes can delay Wait.
func configureCommand(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	cmd.WaitDelay = processWaitDelay
	configureCancellation(cmd)
}

// RunCommand starts and waits for a context-backed command. Its own context
// watcher remains active until Wait finishes, so cancellation still reaches a
// process group when the original child has exited but a descendant holds an
// output pipe open.
func RunCommand(ctx context.Context, cmd *exec.Cmd) error {
	if ctx == nil {
		return errors.New("nil context")
	}
	if cmd == nil {
		return errors.New("nil command")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	configureCommand(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}

	finished := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-ctx.Done():
			if cmd.Cancel != nil {
				_ = cmd.Cancel()
			} else if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		case <-finished:
		}
	}()
	err := cmd.Wait()
	close(finished)
	<-watcherDone
	return err
}

// Run executes command in cwd and captures independently bounded stdout and
// stderr. A normal non-zero exit is returned as an *exec.ExitError with its
// exit status; startup failures and context cancellation have exit status -1.
func Run(ctx context.Context, cwd, command string, args []string) (stdout, stderr string, exitCode int, err error) {
	if ctx == nil {
		return "", "", -1, errors.New("nil context")
	}
	if strings.TrimSpace(command) == "" {
		return "", "", -1, errors.New("empty command")
	}
	if err := ctx.Err(); err != nil {
		return "", "", -1, err
	}

	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = cwd
	var out, errOut cappedBuffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = RunCommand(ctx, cmd)
	stdout, stderr = out.String(), errOut.String()
	if err == nil {
		return stdout, stderr, 0, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return stdout, stderr, -1, ctxErr
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return stdout, stderr, exitErr.ExitCode(), err
	}
	return stdout, stderr, -1, err
}

type cappedBuffer struct {
	buf       bytes.Buffer
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	originalLen := len(p)
	remaining := OutputLimit - b.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
			b.truncated = true
		}
		_, _ = b.buf.Write(p)
	} else if len(p) != 0 {
		b.truncated = true
	}
	// Report the complete write even after the cap. os/exec can then continue
	// draining the pipe instead of turning output truncation into child failure.
	return originalLen, nil
}

func (b *cappedBuffer) String() string {
	if !b.truncated {
		return b.buf.String()
	}
	return b.buf.String() + outputTruncatedMarker
}

var baselineEnvironment = map[string]struct{}{
	"PATH": {}, "HOME": {}, "USER": {}, "LOGNAME": {},
	"TMPDIR": {}, "TMP": {}, "TEMP": {},
	"LANG": {}, "LANGUAGE": {},
	"LC_ALL": {}, "LC_COLLATE": {}, "LC_CTYPE": {}, "LC_MESSAGES": {},
	"LC_MONETARY": {}, "LC_NUMERIC": {}, "LC_TIME": {}, "LC_ADDRESS": {},
	"LC_IDENTIFICATION": {}, "LC_MEASUREMENT": {}, "LC_NAME": {},
	"LC_PAPER": {}, "LC_TELEPHONE": {},
	"TERM": {}, "COLORTERM": {}, "SSH_AUTH_SOCK": {},
}

// MinimalEnvironment returns a deterministic allowlisted subset of the current
// environment. Explicit values are added last conceptually and override an
// inherited baseline key (case-insensitively on Windows). This intentionally
// excludes provider credentials, API/token variables, and CI secrets.
func MinimalEnvironment(overrides map[string]string) []string {
	values := make(map[string]string)
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" || !allowedEnvironmentKey(key) {
			continue
		}
		values[environmentKey(key)] = entry
	}
	for key, value := range overrides {
		if key == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, '\x00') {
			continue
		}
		values[environmentKey(key)] = key + "=" + value
	}

	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	environment := make([]string, 0, len(keys))
	for _, key := range keys {
		environment = append(environment, values[key])
	}
	return environment
}

func allowedEnvironmentKey(key string) bool {
	if runtime.GOOS == "windows" {
		key = strings.ToUpper(key)
	}
	if _, ok := baselineEnvironment[key]; ok {
		return true
	}
	if runtime.GOOS == "windows" {
		switch key {
		case "SYSTEMROOT", "COMSPEC", "PATHEXT":
			return true
		}
	}
	return false
}

func environmentKey(key string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(key)
	}
	return key
}
