package extpkg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	staleLockAge = 30 * time.Minute
	maxStateSize = 8 << 20
)

// Options configures a Store. Non-default fields mainly support tests and
// GitHub Enterprise-compatible API endpoints.
type Options struct {
	Root          string
	Client        *http.Client
	GitHubAPIBase string
	GitPath       string
	Now           func() time.Time
}

// Store owns installed package content and its lock file.
type Store struct {
	root       string
	packages   string
	statePath  string
	lockPath   string
	client     *http.Client
	apiBaseURL string
	gitPath    string
	now        func() time.Time
}

// New creates a store rooted at the Notch XDG data directory.
func New(root string) *Store { return NewWithOptions(Options{Root: root}) }

// NewWithOptions creates a configurable store.
func NewWithOptions(options Options) *Store {
	client := options.Client
	if client == nil {
		client = &http.Client{Timeout: 45 * time.Second}
	}
	apiBase := options.GitHubAPIBase
	if apiBase == "" {
		apiBase = defaultGitHubAPI
	}
	gitPath := options.GitPath
	if gitPath == "" {
		gitPath = "git"
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	root := filepath.Clean(options.Root)
	return &Store{
		root: root, packages: filepath.Join(root, "packages"), statePath: filepath.Join(root, "packages.json"),
		lockPath: filepath.Join(root, ".packages.lock"), client: client, apiBaseURL: apiBase, gitPath: gitPath, now: now,
	}
}

// Install fetches, validates, and atomically installs one package.
func (s *Store) Install(ctx context.Context, source Source) (Installed, error) {
	release, err := s.lock()
	if err != nil {
		return Installed{}, err
	}
	defer release()
	state, err := s.loadState()
	if err != nil {
		return Installed{}, err
	}
	if err := s.recoverTransactions(state); err != nil {
		return Installed{}, err
	}
	prepared, cleanup, err := s.prepare(ctx, source)
	if err != nil {
		return Installed{}, err
	}
	defer cleanup()
	for _, installed := range state.Packages {
		if installed.Name == prepared.manifest.Name {
			return Installed{}, fmt.Errorf("extension package %q is already installed; use `notch extensions update %s`", installed.Name, installed.Name)
		}
	}
	target := filepath.Join(s.packages, prepared.manifest.Name)
	if _, err := os.Lstat(target); err == nil {
		return Installed{}, fmt.Errorf("package destination %q already exists but is not tracked", target)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Installed{}, err
	}
	if err := os.Rename(prepared.path, target); err != nil {
		return Installed{}, fmt.Errorf("install package files: %w", err)
	}
	installedAt := s.now().UTC()
	installed := Installed{
		Name: prepared.manifest.Name, Version: prepared.manifest.Version, Description: prepared.manifest.Description,
		Source: source, Resolved: prepared.resolved, Integrity: prepared.integrity,
		InstalledAt: installedAt, UpdatedAt: installedAt,
	}
	state.Packages = append(state.Packages, installed)
	sortInstalled(state.Packages)
	if err := s.saveState(state); err != nil {
		_ = os.RemoveAll(target)
		return Installed{}, err
	}
	return installed, nil
}

// UpdateResult describes one selected package update.
type UpdateResult struct {
	Before  Installed `json:"before"`
	After   Installed `json:"after"`
	Changed bool      `json:"changed"`
}

// Update refreshes named packages, or every package when names is empty.
func (s *Store) Update(ctx context.Context, names []string, force bool) ([]UpdateResult, error) {
	release, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer release()
	state, err := s.loadState()
	if err != nil {
		return nil, err
	}
	if err := s.recoverTransactions(state); err != nil {
		return nil, err
	}
	indexes, err := selectedIndexes(state.Packages, names)
	if err != nil {
		return nil, err
	}
	results := make([]UpdateResult, 0, len(indexes))
	for _, index := range indexes {
		before := state.Packages[index]
		prepared, cleanup, err := s.prepare(ctx, before.Source)
		if err != nil {
			return results, fmt.Errorf("update %s: %w", before.Name, err)
		}
		if prepared.manifest.Name != before.Name {
			cleanup()
			return results, fmt.Errorf("update %s: source now declares package %q", before.Name, prepared.manifest.Name)
		}
		if !force && compareVersions(prepared.manifest.Version, before.Version) < 0 {
			cleanup()
			return results, fmt.Errorf("update %s would downgrade %s to %s; pass --force to allow it", before.Name, before.Version, prepared.manifest.Version)
		}
		after := before
		after.Version = prepared.manifest.Version
		after.Description = prepared.manifest.Description
		after.Resolved = prepared.resolved
		after.Integrity = prepared.integrity
		result := UpdateResult{Before: before, After: after}
		target := filepath.Join(s.packages, before.Name)
		currentIntegrity, currentErr := treeIntegrity(target)
		currentMatches := currentErr == nil && currentIntegrity == after.Integrity
		if before.Version == after.Version && before.Resolved == after.Resolved && before.Integrity == after.Integrity && currentMatches {
			cleanup()
			results = append(results, result)
			continue
		}
		after.UpdatedAt = s.now().UTC()
		result.After, result.Changed = after, true
		backup := target + ".backup"
		if _, err := os.Lstat(backup); !errors.Is(err, os.ErrNotExist) {
			cleanup()
			if err == nil {
				return results, fmt.Errorf("update %s: stale backup still exists", before.Name)
			}
			return results, err
		}
		backupMoved := false
		if err := os.Rename(target, backup); err == nil {
			backupMoved = true
		} else if !errors.Is(err, os.ErrNotExist) {
			cleanup()
			return results, fmt.Errorf("update %s: preserve current package: %w", before.Name, err)
		}
		if err := os.Rename(prepared.path, target); err != nil {
			if backupMoved {
				_ = os.Rename(backup, target)
			}
			cleanup()
			return results, fmt.Errorf("update %s: install package files: %w", before.Name, err)
		}
		state.Packages[index] = after
		if err := s.saveState(state); err != nil {
			_ = os.RemoveAll(target)
			if backupMoved {
				_ = os.Rename(backup, target)
			}
			cleanup()
			return results, err
		}
		cleanup()
		if backupMoved {
			if err := os.RemoveAll(backup); err != nil {
				return results, fmt.Errorf("update %s succeeded but old package cleanup failed: %w", before.Name, err)
			}
		}
		results = append(results, result)
	}
	return results, nil
}

// Remove atomically removes one tracked package.
func (s *Store) Remove(name string) (Installed, error) {
	if !packageNamePattern.MatchString(name) {
		return Installed{}, fmt.Errorf("invalid package name %q", name)
	}
	release, err := s.lock()
	if err != nil {
		return Installed{}, err
	}
	defer release()
	state, err := s.loadState()
	if err != nil {
		return Installed{}, err
	}
	if err := s.recoverTransactions(state); err != nil {
		return Installed{}, err
	}
	index := -1
	var removed Installed
	for i, installed := range state.Packages {
		if installed.Name == name {
			index, removed = i, installed
			break
		}
	}
	if index < 0 {
		return Installed{}, fmt.Errorf("extension package %q is not installed", name)
	}
	target := filepath.Join(s.packages, name)
	backup := target + ".remove"
	if _, err := os.Lstat(backup); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return Installed{}, fmt.Errorf("remove %s: stale removal backup still exists", name)
		}
		return Installed{}, err
	}
	moved := false
	if err := os.Rename(target, backup); err == nil {
		moved = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return Installed{}, fmt.Errorf("remove package files: %w", err)
	}
	state.Packages = append(state.Packages[:index], state.Packages[index+1:]...)
	if err := s.saveState(state); err != nil {
		if moved {
			_ = os.Rename(backup, target)
		}
		return Installed{}, err
	}
	if moved {
		if err := os.RemoveAll(backup); err != nil {
			return removed, fmt.Errorf("package was removed from state but cleanup failed: %w", err)
		}
	}
	return removed, nil
}

// List returns installed packages and detects local modifications.
func (s *Store) List() ([]Status, error) {
	release, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer release()
	state, err := s.loadState()
	if err != nil {
		return nil, err
	}
	if err := s.recoverTransactions(state); err != nil {
		return nil, err
	}
	statuses := make([]Status, 0, len(state.Packages))
	for _, installed := range state.Packages {
		status := Status{Installed: installed, State: "ok"}
		integrity, err := treeIntegrity(filepath.Join(s.packages, installed.Name))
		if errors.Is(err, os.ErrNotExist) {
			status.State = "missing"
		} else if err != nil {
			status.State = "unreadable"
		} else if integrity != installed.Integrity {
			status.State = "modified"
		}
		statuses = append(statuses, status)
	}
	return statuses, nil
}

// DiscoveryDirs returns validated extension resource directories in stable
// package and manifest order. A missing state file means no installed packages.
func DiscoveryDirs(root string) ([]string, error) {
	store := New(root)
	release, err := store.lock()
	if err != nil {
		return nil, err
	}
	defer release()
	state, err := store.loadState()
	if err != nil {
		return nil, err
	}
	if err := store.recoverTransactions(state); err != nil {
		return nil, err
	}
	var directories []string
	for _, installed := range state.Packages {
		packageRoot := filepath.Join(store.packages, installed.Name)
		manifest, err := loadManifest(packageRoot)
		if err != nil {
			return nil, fmt.Errorf("load installed package %s: %w", installed.Name, err)
		}
		if manifest.Name != installed.Name {
			return nil, fmt.Errorf("installed package directory %s declares name %q", installed.Name, manifest.Name)
		}
		for _, directory := range manifest.Extensions {
			directories = append(directories, filepath.Join(packageRoot, filepath.FromSlash(directory)))
		}
	}
	return directories, nil
}

type preparedPackage struct {
	path      string
	root      string
	manifest  Manifest
	resolved  string
	integrity string
}

func (s *Store) prepare(ctx context.Context, source Source) (preparedPackage, func(), error) {
	if err := validateSource(source); err != nil {
		return preparedPackage{}, func() {}, fmt.Errorf("invalid package source: %w", err)
	}
	if err := secureDirectory(s.packages); err != nil {
		return preparedPackage{}, func() {}, fmt.Errorf("create package directory: %w", err)
	}
	stageRoot, err := os.MkdirTemp(s.packages, ".stage-")
	if err != nil {
		return preparedPackage{}, func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(stageRoot) }
	packagePath := filepath.Join(stageRoot, "package")
	resolved, err := s.fetchSource(ctx, source, packagePath)
	if err != nil {
		cleanup()
		return preparedPackage{}, func() {}, err
	}
	if err := validatePackageTree(packagePath); err != nil {
		cleanup()
		return preparedPackage{}, func() {}, fmt.Errorf("validate package files: %w", err)
	}
	manifest, err := loadManifest(packagePath)
	if err != nil {
		cleanup()
		return preparedPackage{}, func() {}, err
	}
	integrity, err := treeIntegrity(packagePath)
	if err != nil {
		cleanup()
		return preparedPackage{}, func() {}, fmt.Errorf("hash package: %w", err)
	}
	if resolved == "local" {
		resolved = integrity
	}
	return preparedPackage{path: packagePath, root: stageRoot, manifest: manifest, resolved: resolved, integrity: integrity}, cleanup, nil
}

func (s *Store) loadState() (stateFile, error) {
	oldPath := s.statePath + ".old"
	if _, err := os.Stat(s.statePath); errors.Is(err, os.ErrNotExist) {
		if _, oldErr := os.Stat(oldPath); oldErr == nil {
			if renameErr := os.Rename(oldPath, s.statePath); renameErr != nil {
				return stateFile{}, fmt.Errorf("recover extension package state: %w", renameErr)
			}
		} else if !errors.Is(oldErr, os.ErrNotExist) {
			return stateFile{}, oldErr
		}
	} else if err != nil {
		return stateFile{}, err
	} else {
		_ = os.Remove(oldPath)
	}
	file, err := os.Open(s.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return stateFile{Version: stateVersion}, nil
	}
	if err != nil {
		return stateFile{}, fmt.Errorf("open extension package state: %w", err)
	}
	defer file.Close()
	if info, err := file.Stat(); err != nil {
		return stateFile{}, fmt.Errorf("inspect extension package state: %w", err)
	} else if info.Size() > maxStateSize {
		return stateFile{}, fmt.Errorf("extension package state exceeds %d bytes", maxStateSize)
	}
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var state stateFile
	if err := decoder.Decode(&state); err != nil {
		return stateFile{}, fmt.Errorf("decode extension package state: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return stateFile{}, fmt.Errorf("decode extension package state: %w", err)
	}
	if state.Version != stateVersion {
		return stateFile{}, fmt.Errorf("unsupported extension package state version %d", state.Version)
	}
	seen := make(map[string]bool, len(state.Packages))
	for _, installed := range state.Packages {
		if len(installed.Name) > 128 || !packageNamePattern.MatchString(installed.Name) || len(installed.Version) > 128 || !versionPattern.MatchString(installed.Version) || !integrityPattern.MatchString(installed.Integrity) || installed.Resolved == "" || len(installed.Resolved) > 256 || len(installed.Description) > 4096 || installed.InstalledAt.IsZero() || installed.UpdatedAt.IsZero() {
			return stateFile{}, fmt.Errorf("invalid installed package record for %q", installed.Name)
		}
		if _, ok := parsePackageVersion(installed.Version); !ok {
			return stateFile{}, fmt.Errorf("invalid installed package version for %q", installed.Name)
		}
		if err := validateSource(installed.Source); err != nil {
			return stateFile{}, fmt.Errorf("invalid source for installed package %q: %w", installed.Name, err)
		}
		if seen[installed.Name] {
			return stateFile{}, fmt.Errorf("duplicate installed package %q", installed.Name)
		}
		seen[installed.Name] = true
	}
	sortInstalled(state.Packages)
	return state, nil
}

func secureDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}

func (s *Store) saveState(state stateFile) error {
	if err := secureDirectory(s.root); err != nil {
		return err
	}
	sortInstalled(state.Packages)
	contents, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	contents = append(contents, '\n')
	temporary, err := os.CreateTemp(s.root, ".packages-*.tmp")
	if err != nil {
		return err
	}
	path := temporary.Name()
	cleanup := true
	defer func() {
		_ = temporary.Close()
		if cleanup {
			_ = os.Remove(path)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		backup := s.statePath + ".old"
		_ = os.Remove(backup)
		hadState := false
		if err := os.Rename(s.statePath, backup); err == nil {
			hadState = true
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.Rename(path, s.statePath); err != nil {
			if hadState {
				_ = os.Rename(backup, s.statePath)
			}
			return err
		}
		_ = os.Remove(backup)
	} else if err := os.Rename(path, s.statePath); err != nil {
		return err
	}
	cleanup = false
	if directory, err := os.Open(s.root); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}

func (s *Store) lock() (func(), error) {
	if err := secureDirectory(s.root); err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		file, err := os.OpenFile(s.lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = fmt.Fprintf(file, "%d\n", os.Getpid())
			_ = file.Close()
			return func() { _ = os.Remove(s.lockPath) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		contents, readErr := os.ReadFile(s.lockPath)
		pid, parseErr := strconv.Atoi(strings.TrimSpace(string(contents)))
		if readErr == nil && parseErr == nil {
			if alive, known := processAlive(pid); known {
				if !alive {
					if removeErr := os.Remove(s.lockPath); removeErr == nil {
						continue
					}
				}
				return nil, fmt.Errorf("another extension package operation is running (lock: %s)", s.lockPath)
			}
		}
		info, statErr := os.Stat(s.lockPath)
		if statErr == nil && s.now().Sub(info.ModTime()) > staleLockAge {
			if removeErr := os.Remove(s.lockPath); removeErr == nil {
				continue
			}
		}
		return nil, fmt.Errorf("another extension package operation is running (lock: %s)", s.lockPath)
	}
	return nil, errors.New("could not acquire extension package lock")
}

func (s *Store) recoverTransactions(state stateFile) error {
	if err := secureDirectory(s.packages); err != nil {
		return err
	}
	tracked := make(map[string]Installed, len(state.Packages))
	for _, installed := range state.Packages {
		tracked[installed.Name] = installed
	}
	entries, err := os.ReadDir(s.packages)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".stage-") {
			if err := os.RemoveAll(filepath.Join(s.packages, name)); err != nil {
				return fmt.Errorf("clean stale package staging directory: %w", err)
			}
			continue
		}
		var suffix string
		switch {
		case strings.HasSuffix(name, ".backup"):
			suffix = ".backup"
		case strings.HasSuffix(name, ".remove"):
			suffix = ".remove"
		default:
			if _, ok := tracked[name]; entry.IsDir() && packageNamePattern.MatchString(name) && !ok {
				if err := os.RemoveAll(filepath.Join(s.packages, name)); err != nil {
					return fmt.Errorf("clean untracked package directory %s: %w", name, err)
				}
			}
			continue
		}
		packageName := strings.TrimSuffix(name, suffix)
		if !packageNamePattern.MatchString(packageName) {
			continue
		}
		backup, target := filepath.Join(s.packages, name), filepath.Join(s.packages, packageName)
		installed, isTracked := tracked[packageName]
		_, targetErr := os.Lstat(target)
		if isTracked && errors.Is(targetErr, os.ErrNotExist) {
			if err := os.Rename(backup, target); err != nil {
				return fmt.Errorf("recover package %s: %w", packageName, err)
			}
			continue
		}
		if suffix == ".backup" && isTracked && targetErr == nil {
			targetIntegrity, targetHashErr := treeIntegrity(target)
			if targetHashErr == nil && targetIntegrity == installed.Integrity {
				if err := os.RemoveAll(backup); err != nil {
					return fmt.Errorf("clean package transaction %s: %w", packageName, err)
				}
				continue
			}
			backupIntegrity, backupHashErr := treeIntegrity(backup)
			if backupHashErr == nil && backupIntegrity == installed.Integrity {
				if err := os.RemoveAll(target); err != nil {
					return fmt.Errorf("roll back package %s: %w", packageName, err)
				}
				if err := os.Rename(backup, target); err != nil {
					return fmt.Errorf("restore package %s: %w", packageName, err)
				}
				continue
			}
			return fmt.Errorf("cannot recover package %s: neither current nor backup content matches package state", packageName)
		}
		if err := os.RemoveAll(backup); err != nil {
			return fmt.Errorf("clean package transaction %s: %w", packageName, err)
		}
	}
	return nil
}

func selectedIndexes(packages []Installed, names []string) ([]int, error) {
	if len(names) == 0 {
		indexes := make([]int, len(packages))
		for i := range packages {
			indexes[i] = i
		}
		return indexes, nil
	}
	wanted := make(map[string]bool, len(names))
	for _, name := range names {
		if !packageNamePattern.MatchString(name) {
			return nil, fmt.Errorf("invalid package name %q", name)
		}
		wanted[name] = true
	}
	var indexes []int
	for i, installed := range packages {
		if wanted[installed.Name] {
			indexes = append(indexes, i)
			delete(wanted, installed.Name)
		}
	}
	if len(wanted) != 0 {
		missing := make([]string, 0, len(wanted))
		for name := range wanted {
			missing = append(missing, name)
		}
		sort.Strings(missing)
		return nil, fmt.Errorf("extension package is not installed: %s", strings.Join(missing, ", "))
	}
	return indexes, nil
}
