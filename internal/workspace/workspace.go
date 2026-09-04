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

const (
	trustFileName       = "trusted-workspaces.json"
	maxInstructionsSize = 1 << 20
)

// Info separates the active worktree root, where project inputs are loaded,
// from the repository-wide trust key shared by all linked worktrees. Branch is
// the current symbolic branch, or empty outside Git and in detached HEAD mode.
type Info struct {
	Root     string
	TrustKey string
	Branch   string
}

// Resolve identifies the workspace containing cwd. Git worktrees use their
// own top-level directory as Root and their shared Git common directory as
// TrustKey. Outside Git, both values are the canonical cwd.
func Resolve(cwd string) (Info, error) {
	canonicalCWD, err := canonicalPath(cwd)
	if err != nil {
		return Info{}, fmt.Errorf("canonicalize workspace: %w", err)
	}
	cmd := exec.Command("git", "-C", canonicalCWD, "rev-parse", "--show-toplevel", "--git-common-dir", "--git-path", "HEAD")
	cmd.Env = gitDiscoveryEnv(os.Environ())
	cmd.Stderr = io.Discard
	if output, runErr := cmd.Output(); runErr == nil {
		lines := strings.Split(strings.TrimSpace(string(output)), "\n")
		if len(lines) == 3 && strings.TrimSpace(lines[0]) != "" && strings.TrimSpace(lines[1]) != "" {
			root, canonicalErr := canonicalPath(strings.TrimSpace(lines[0]))
			if canonicalErr != nil {
				return Info{}, fmt.Errorf("canonicalize Git workspace: %w", canonicalErr)
			}
			common := strings.TrimSpace(lines[1])
			if !filepath.IsAbs(common) {
				common = filepath.Join(canonicalCWD, common)
			}
			trustKey, canonicalErr := canonicalPath(common)
			if canonicalErr != nil {
				return Info{}, fmt.Errorf("canonicalize Git common directory: %w", canonicalErr)
			}
			branch := ""
			headPath := strings.TrimSpace(lines[2])
			if !filepath.IsAbs(headPath) {
				headPath = filepath.Join(canonicalCWD, headPath)
			}
			if head, readErr := os.ReadFile(headPath); readErr == nil {
				const branchPrefix = "ref: refs/heads/"
				if ref := strings.TrimSpace(string(head)); strings.HasPrefix(ref, branchPrefix) {
					branch = strings.TrimPrefix(ref, branchPrefix)
				}
			}
			return Info{Root: root, TrustKey: trustKey, Branch: branch}, nil
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
				return Info{Root: candidate, TrustKey: marker}, nil
			case info.Mode().IsRegular():
				trustKey, resolveErr := worktreeCommonDir(marker)
				if resolveErr != nil {
					return Info{}, fmt.Errorf("inspect Git workspace: %w", resolveErr)
				}
				return Info{Root: candidate, TrustKey: trustKey}, nil
			default:
				return Info{}, fmt.Errorf("inspect Git workspace: .git marker %q is not a directory or regular worktree file", marker)
			}
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return Info{}, fmt.Errorf("inspect Git workspace: %w", statErr)
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			break
		}
	}
	return Info{Root: canonicalCWD, TrustKey: canonicalCWD}, nil
}

// Root returns the canonical active worktree root containing cwd.
func Root(cwd string) (string, error) {
	info, err := Resolve(cwd)
	return info.Root, err
}

func worktreeCommonDir(marker string) (string, error) {
	data, err := os.ReadFile(marker)
	if err != nil {
		return "", err
	}
	line := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	if strings.ContainsAny(line, "\r\n") {
		return "", fmt.Errorf("malformed .git file %q", marker)
	}
	gitDir, found := strings.CutPrefix(line, "gitdir:")
	gitDir = strings.TrimSpace(gitDir)
	if !found || gitDir == "" {
		return "", fmt.Errorf("malformed .git file %q", marker)
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(filepath.Dir(marker), gitDir)
	}
	gitDir, err = canonicalPath(gitDir)
	if err != nil {
		return "", fmt.Errorf("canonicalize Git directory: %w", err)
	}
	commonData, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
	if err != nil {
		return "", fmt.Errorf("read Git common directory: %w", err)
	}
	common := strings.TrimSpace(string(commonData))
	if common == "" || strings.ContainsAny(common, "\r\n") {
		return "", errors.New("malformed Git common directory")
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(gitDir, common)
	}
	trustKey, err := canonicalPath(common)
	if err != nil {
		return "", fmt.Errorf("canonicalize Git common directory: %w", err)
	}
	return trustKey, nil
}

// HasProjectInputs reports whether root contains any supported project config,
// MCP config, extension, resource, theme, or .agents input. Empty discovery
// directories are not inputs.
func HasProjectInputs(root string) (bool, error) {
	for _, path := range []string{
		filepath.Join(root, ".notch", "config.json"),
		filepath.Join(root, ".notch", "mcp.json"),
		filepath.Join(root, "AGENTS.md"),
		filepath.Join(root, "AGENTS.local.md"),
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

// Instructions loads trusted root-level agent instructions in precedence
// order. AGENTS.local.md is appended last so local guidance can override the
// shared AGENTS.md guidance.
func Instructions(root string) (string, error) {
	var sections []string
	total := 0
	for _, name := range []string{"AGENTS.md", "AGENTS.local.md"} {
		path := filepath.Join(root, name)
		data, err := readInstructionFile(path, maxInstructionsSize-total)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("read workspace instructions %q: %w", path, err)
		}
		total += len(data)
		content := strings.TrimSpace(string(data))
		if content != "" {
			sections = append(sections, "## Workspace instructions from "+name+"\n\n"+content)
		}
	}
	return strings.Join(sections, "\n\n"), nil
}

func readInstructionFile(path string, remaining int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, int64(remaining)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > remaining {
		return nil, fmt.Errorf("workspace instructions exceed %d bytes", maxInstructionsSize)
	}
	return data, nil
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

// NewStore creates a trust store rooted at the Notch XDG data directory.
func NewStore(dataRoot string) *Store {
	return &Store{home: filepath.Clean(dataRoot)}
}

// Path returns the trust database path.
func (s *Store) Path() string {
	return filepath.Join(s.home, trustFileName)
}

// IsTrusted reports whether the workspace containing path is trusted. Git
// repository trust is shared by all linked worktrees.
func (s *Store) IsTrusted(path string) (bool, error) {
	info, err := Resolve(path)
	if err != nil {
		return false, fmt.Errorf("resolve trusted workspace: %w", err)
	}
	return s.IsTrustedWorkspace(info.Root, info.TrustKey)
}

// IsTrustedWorkspace reports whether trust exists for the repository-wide key.
// It also recognizes legacy worktree-root records and migrates a matching
// record to the shared key.
func (s *Store) IsTrustedWorkspace(root, trustKey string) (bool, error) {
	doc, err := s.read()
	if err != nil {
		return false, err
	}
	legacyMatch := false
	for _, trusted := range doc.Workspaces {
		if trusted == trustKey {
			return true, nil
		}
		if trusted == root {
			legacyMatch = true
			continue
		}
		info, resolveErr := Resolve(trusted)
		if resolveErr == nil && info.TrustKey == trustKey {
			legacyMatch = true
		}
	}
	if !legacyMatch {
		return false, nil
	}
	if err := s.TrustRoot(trustKey); err != nil {
		return false, fmt.Errorf("migrate workspace trust: %w", err)
	}
	return true, nil
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

// Trust atomically persists trust for the workspace containing path. Git
// repository trust is shared by all linked worktrees.
func (s *Store) Trust(path string) error {
	info, err := Resolve(path)
	if err != nil {
		return fmt.Errorf("resolve trusted workspace: %w", err)
	}
	return s.TrustRoot(info.TrustKey)
}

// TrustRoot atomically persists the exact canonical root as trusted. The Notch
// data directory and trust database are forced to owner-only permissions. Unlike normal
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
