// Package workspace identifies project workspaces and stores explicit trust in
// project-controlled inputs.
package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const trustFileName = "trusted-workspaces.json"

// Root returns the canonical root of the Git workspace containing cwd. Outside
// a Git worktree it returns the canonical cwd.
func Root(cwd string) (string, error) {
	canonicalCWD, err := canonicalPath(cwd)
	if err != nil {
		return "", fmt.Errorf("canonicalize workspace: %w", err)
	}
	cmd := exec.Command("git", "-C", canonicalCWD, "rev-parse", "--show-toplevel")
	cmd.Env = gitDiscoveryEnv(os.Environ())
	cmd.Stderr = io.Discard
	if output, runErr := cmd.Output(); runErr == nil {
		root := strings.TrimSpace(string(output))
		if root != "" {
			canonicalRoot, canonicalErr := canonicalPath(root)
			if canonicalErr != nil {
				return "", fmt.Errorf("canonicalize Git workspace: %w", canonicalErr)
			}
			return canonicalRoot, nil
		}
	}

	// Keep workspace identity useful when Git is unavailable. A .git file is
	// used by linked worktrees, while a .git directory is used by normal ones.
	// Refuse symlinks and special/malformed markers instead of allowing an
	// untrusted marker to redefine the workspace boundary.
	for candidate := canonicalCWD; ; candidate = filepath.Dir(candidate) {
		marker := filepath.Join(candidate, ".git")
		info, statErr := os.Lstat(marker)
		if statErr == nil {
			switch {
			case info.IsDir():
				return candidate, nil
			case info.Mode().IsRegular():
				valid, validateErr := validWorktreeMarker(marker)
				if validateErr != nil {
					return "", fmt.Errorf("inspect Git workspace: %w", validateErr)
				}
				if valid {
					return candidate, nil
				}
				return "", fmt.Errorf("inspect Git workspace: malformed .git file %q", marker)
			default:
				return "", fmt.Errorf("inspect Git workspace: .git marker %q is not a directory or regular worktree file", marker)
			}
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return "", fmt.Errorf("inspect Git workspace: %w", statErr)
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			break
		}
	}
	return canonicalCWD, nil
}

func validWorktreeMarker(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	line := strings.TrimSuffix(string(data), "\n")
	line = strings.TrimSuffix(line, "\r")
	if strings.ContainsAny(line, "\r\n") {
		return false, nil
	}
	value, found := strings.CutPrefix(line, "gitdir:")
	return found && strings.TrimSpace(value) != "", nil
}

// HasProjectInputs reports whether root contains any supported project config,
// MCP config, extension, resource, theme, or .agents input. Empty discovery
// directories are not inputs.
func HasProjectInputs(root string) (bool, error) {
	for _, path := range []string{
		filepath.Join(root, ".notch", "config.json"),
		filepath.Join(root, ".notch", "mcp.json"),
	} {
		if _, err := os.Lstat(path); err == nil {
			return true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("inspect project input %q: %w", path, err)
		}
	}
	for _, path := range []string{
		filepath.Join(root, ".notch", "extensions"),
		filepath.Join(root, ".notch", "skills"),
		filepath.Join(root, ".notch", "prompts"),
		filepath.Join(root, ".notch", "themes"),
		filepath.Join(root, ".agents", "skills"),
		filepath.Join(root, ".agents", "commands"),
	} {
		present, err := nonEmptyPath(path)
		if err != nil {
			return false, fmt.Errorf("inspect project input %q: %w", path, err)
		}
		if present {
			return true, nil
		}
	}
	return false, nil
}

func nonEmptyPath(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	// A symlink or unexpected file is itself project-controlled input. Do not
	// follow it while deciding whether to ask for trust.
	if !info.IsDir() {
		return true, nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
			return true, nil
		}
		present, err := nonEmptyPath(filepath.Join(path, entry.Name()))
		if err != nil {
			return false, err
		}
		if present {
			return true, nil
		}
	}
	return false, nil
}

// Store persists canonical workspace roots below the Notch home directory.
type Store struct {
	home string
}

// NewStore creates a trust store rooted at notchHome.
func NewStore(notchHome string) *Store {
	return &Store{home: filepath.Clean(notchHome)}
}

// Path returns the trust database path.
func (s *Store) Path() string {
	return filepath.Join(s.home, trustFileName)
}

// IsTrusted reports whether the canonical workspace containing path is present
// in the trust database. It is a convenience wrapper around IsTrustedRoot.
func (s *Store) IsTrusted(path string) (bool, error) {
	root, err := Root(path)
	if err != nil {
		return false, fmt.Errorf("resolve trusted workspace root: %w", err)
	}
	return s.IsTrustedRoot(root)
}

// IsTrustedRoot reports whether the exact canonical root is present in the
// trust database. It does not perform workspace discovery, allowing callers to
// use the same root for trust decisions and subsequent project input loading.
func (s *Store) IsTrustedRoot(root string) (bool, error) {
	doc, err := s.read()
	if err != nil {
		return false, err
	}
	for _, trusted := range doc.Workspaces {
		if trusted == root {
			return true, nil
		}
	}
	return false, nil
}

// Trust atomically persists the canonical workspace containing path. It is a
// convenience wrapper around TrustRoot.
func (s *Store) Trust(path string) error {
	root, err := Root(path)
	if err != nil {
		return fmt.Errorf("resolve trusted workspace root: %w", err)
	}
	return s.TrustRoot(root)
}

// TrustRoot atomically persists the exact canonical root as trusted. The Notch
// home and trust database are forced to owner-only permissions. Unlike normal
// trust reads, this explicit trust operation repairs an unsafe regular trust
// database mode before reading it.
func (s *Store) TrustRoot(root string) error {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return fmt.Errorf("trusted workspace root %q is not an absolute clean path", root)
	}
	if err := os.MkdirAll(s.home, 0o700); err != nil {
		return fmt.Errorf("create trust directory %q: %w", s.home, err)
	}
	if info, err := os.Lstat(s.home); err != nil {
		return fmt.Errorf("inspect trust directory %q: %w", s.home, err)
	} else if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("trust directory %q is not a directory", s.home)
	}
	if err := os.Chmod(s.home, 0o700); err != nil {
		return fmt.Errorf("secure trust directory %q: %w", s.home, err)
	}
	if info, err := os.Lstat(s.Path()); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("workspace trust database is not a regular file")
		}
		if err := os.Chmod(s.Path(), 0o600); err != nil {
			return fmt.Errorf("secure workspace trust database: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect workspace trust database: %w", err)
	}
	doc, err := s.read()
	if err != nil {
		return err
	}
	for _, trusted := range doc.Workspaces {
		if trusted == root {
			return nil
		}
	}
	doc.Version = 1
	doc.Workspaces = append(doc.Workspaces, root)
	sort.Strings(doc.Workspaces)
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode workspace trust database: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(s.home, ".trusted-workspaces-*")
	if err != nil {
		return fmt.Errorf("create workspace trust database: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("secure workspace trust database: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write workspace trust database: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync workspace trust database: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close workspace trust database: %w", err)
	}
	if err := os.Rename(tmpPath, s.Path()); err != nil {
		return fmt.Errorf("install workspace trust database: %w", err)
	}
	if err := os.Chmod(s.Path(), 0o600); err != nil {
		return fmt.Errorf("secure workspace trust database: %w", err)
	}
	return nil
}

type trustDocument struct {
	Version    int      `json:"version"`
	Workspaces []string `json:"workspaces"`
}

func (s *Store) read() (trustDocument, error) {
	info, err := os.Lstat(s.Path())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return trustDocument{Version: 1}, nil
		}
		return trustDocument{}, fmt.Errorf("inspect workspace trust database: %w", err)
	}
	if !info.Mode().IsRegular() {
		return trustDocument{}, errors.New("workspace trust database is not a regular file")
	}
	if info.Mode().Perm() != 0o600 {
		return trustDocument{}, fmt.Errorf("workspace trust database %q must have mode 0600", s.Path())
	}
	if homeInfo, homeErr := os.Stat(s.home); homeErr != nil {
		return trustDocument{}, fmt.Errorf("inspect workspace trust directory: %w", homeErr)
	} else if !homeInfo.IsDir() || homeInfo.Mode().Perm() != 0o700 {
		return trustDocument{}, fmt.Errorf("workspace trust directory %q must have mode 0700", s.home)
	}
	data, err := os.ReadFile(s.Path())
	if err != nil {
		return trustDocument{}, fmt.Errorf("read workspace trust database: %w", err)
	}
	var doc trustDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		return trustDocument{}, fmt.Errorf("parse workspace trust database: %w", err)
	}
	if doc.Version != 1 {
		return trustDocument{}, fmt.Errorf("unsupported workspace trust database version %d", doc.Version)
	}
	return doc, nil
}

func gitDiscoveryEnv(environment []string) []string {
	blocked := map[string]bool{
		"GIT_DIR": true, "GIT_WORK_TREE": true, "GIT_COMMON_DIR": true,
		"GIT_OBJECT_DIRECTORY": true, "GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
		"GIT_INDEX_FILE": true, "GIT_CEILING_DIRECTORIES": true,
	}
	clean := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		if !blocked[name] {
			clean = append(clean, entry)
		}
	}
	return append(clean, "GIT_OPTIONAL_LOCKS=0")
}

func canonicalPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	return filepath.Clean(canonical), nil
}
