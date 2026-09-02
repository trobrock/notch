package upgrade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const DefaultCheckInterval = 24 * time.Hour

// AutomaticOptions controls a rate-limited automatic upgrade check.
type AutomaticOptions struct {
	Upgrade   Options
	StatePath string
	Interval  time.Duration
	Now       func() time.Time
}

// Automatic checks for and installs a newer release when the check interval
// has elapsed. The check time is recorded before contacting GitHub so a failed
// check does not delay every subsequent startup.
func Automatic(ctx context.Context, options AutomaticOptions) (Result, bool, error) {
	if _, err := parseSemVersion(options.Upgrade.CurrentVersion); err != nil || isDevelopmentVersion(options.Upgrade.CurrentVersion) {
		// Development and dirty checkout builds must never replace themselves.
		return Result{}, false, nil
	}
	if options.StatePath == "" {
		return Result{}, false, errors.New("automatic update state path is empty")
	}
	interval := options.Interval
	if interval <= 0 {
		interval = DefaultCheckInterval
	}
	now := time.Now
	if options.Now != nil {
		now = options.Now
	}
	checkedAt, err := readAutomaticState(options.StatePath)
	if err != nil {
		return Result{}, false, err
	}
	currentTime := now().UTC()
	if !checkedAt.IsZero() && !checkedAt.After(currentTime) && currentTime.Sub(checkedAt) < interval {
		return Result{}, false, nil
	}
	if err := writeAutomaticState(options.StatePath, currentTime); err != nil {
		return Result{}, false, err
	}
	result, err := Run(ctx, options.Upgrade)
	return result, true, err
}

type automaticState struct {
	LastCheck time.Time `json:"last_check"`
}

func isDevelopmentVersion(version string) bool {
	version = strings.TrimSpace(version)
	if strings.HasSuffix(version, "-dirty") {
		return true
	}
	parts := strings.Split(version, "-")
	if len(parts) < 3 || !isNumeric(parts[len(parts)-2]) {
		return false
	}
	commit := parts[len(parts)-1]
	if len(commit) < 2 || commit[0] != 'g' {
		return false
	}
	for _, r := range commit[1:] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

func readAutomaticState(path string) (time.Time, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("read automatic update state: %w", err)
	}
	var state automaticState
	if err := json.Unmarshal(data, &state); err != nil {
		// A corrupt cache should cause a fresh check rather than break startup.
		return time.Time{}, nil
	}
	return state.LastCheck, nil
}

func writeAutomaticState(path string, checkedAt time.Time) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create automatic update state directory: %w", err)
	}
	data, err := json.Marshal(automaticState{LastCheck: checkedAt})
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(filepath.Dir(path), ".auto-update-*")
	if err != nil {
		return fmt.Errorf("create automatic update state: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure automatic update state: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write automatic update state: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close automatic update state: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		// Windows cannot replace an existing destination with os.Rename.
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return fmt.Errorf("replace automatic update state: %w", err)
		}
		if retryErr := os.Rename(temporaryPath, path); retryErr != nil {
			return fmt.Errorf("replace automatic update state: %w", retryErr)
		}
	}
	return nil
}
