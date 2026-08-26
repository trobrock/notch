package extpkg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

const maxDesiredManifestSize = 1 << 20

// DesiredManifest declares extension packages that should be installed.
type DesiredManifest struct {
	Version  int              `json:"version"`
	Packages []DesiredPackage `json:"packages"`
}

// DesiredPackage identifies one package by its stable manifest name and source.
type DesiredPackage struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	Ref    string `json:"ref,omitempty"`
	Subdir string `json:"subdir,omitempty"`
}

// Desired resolves a portable manifest entry into an installable source.
type Desired struct {
	Name   string `json:"name"`
	Source Source `json:"source"`
}

// SyncResult describes the result of reconciling one desired package.
type SyncResult struct {
	Name      string     `json:"name"`
	Action    string     `json:"action"`
	Installed *Installed `json:"installed,omitempty"`
}

// LoadDesiredManifest strictly loads and validates a dotfiles-friendly package manifest.
// Relative local sources are resolved from the manifest's directory.
func LoadDesiredManifest(path string) ([]Desired, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open extension manifest %q: %w", path, err)
	}
	defer file.Close()
	if info, err := file.Stat(); err != nil {
		return nil, fmt.Errorf("inspect extension manifest %q: %w", path, err)
	} else if info.Size() > maxDesiredManifestSize {
		return nil, fmt.Errorf("extension manifest %q exceeds %d bytes", path, maxDesiredManifestSize)
	}
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var manifest DesiredManifest
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("decode extension manifest %q: %w", path, err)
	}
	if err := ensureDesiredJSONEOF(decoder); err != nil {
		return nil, fmt.Errorf("decode extension manifest %q: %w", path, err)
	}
	if manifest.Version != 1 {
		return nil, fmt.Errorf("extension manifest %q has unsupported version %d", path, manifest.Version)
	}
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("resolve extension manifest directory: %w", err)
	}
	desired := make([]Desired, 0, len(manifest.Packages))
	seen := make(map[string]bool, len(manifest.Packages))
	for index, entry := range manifest.Packages {
		if !packageNamePattern.MatchString(entry.Name) {
			return nil, fmt.Errorf("extension manifest package %d has invalid name %q", index+1, entry.Name)
		}
		if seen[entry.Name] {
			return nil, fmt.Errorf("extension manifest contains duplicate package %q", entry.Name)
		}
		seen[entry.Name] = true
		source, err := ParseSource(entry.Source, base, entry.Ref, entry.Subdir)
		if err != nil {
			return nil, fmt.Errorf("extension manifest package %q: %w", entry.Name, err)
		}
		desired = append(desired, Desired{Name: entry.Name, Source: source})
	}
	sort.Slice(desired, func(i, j int) bool { return desired[i].Name < desired[j].Name })
	return desired, nil
}

func ensureDesiredJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return err
	}
	return errors.New("unexpected data after JSON document")
}

// SyncMissing installs packages declared in desired that are not yet installed.
// Existing packages are left unchanged, but a source mismatch is reported.
func (s *Store) SyncMissing(ctx context.Context, desired []Desired, dryRun bool) ([]SyncResult, error) {
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
	installedByName := make(map[string]Installed, len(state.Packages))
	for _, installed := range state.Packages {
		installedByName[installed.Name] = installed
	}
	results := make([]SyncResult, 0, len(desired))
	for _, want := range desired {
		if current, ok := installedByName[want.Name]; ok && current.Source != want.Source {
			return nil, fmt.Errorf("extension package %q is installed from %s, but the manifest declares %s", want.Name, current.Source.String(), want.Source.String())
		}
	}
	for _, want := range desired {
		if current, ok := installedByName[want.Name]; ok {
			results = append(results, SyncResult{Name: want.Name, Action: "unchanged", Installed: &current})
			continue
		}
		if dryRun {
			results = append(results, SyncResult{Name: want.Name, Action: "install"})
			continue
		}
		prepared, cleanup, err := s.prepare(ctx, want.Source)
		if err != nil {
			return results, fmt.Errorf("install %s: %w", want.Name, err)
		}
		if prepared.manifest.Name != want.Name {
			cleanup()
			return results, fmt.Errorf("install %s: source declares package %q", want.Name, prepared.manifest.Name)
		}
		target := filepath.Join(s.packages, want.Name)
		if _, err := os.Lstat(target); err == nil {
			cleanup()
			return results, fmt.Errorf("package destination %q already exists but is not tracked", target)
		} else if !errors.Is(err, os.ErrNotExist) {
			cleanup()
			return results, err
		}
		if err := os.Rename(prepared.path, target); err != nil {
			cleanup()
			return results, fmt.Errorf("install %s package files: %w", want.Name, err)
		}
		now := s.now().UTC()
		installed := Installed{Name: want.Name, Version: prepared.manifest.Version, Description: prepared.manifest.Description, Source: want.Source, Resolved: prepared.resolved, Integrity: prepared.integrity, InstalledAt: now, UpdatedAt: now}
		state.Packages = append(state.Packages, installed)
		sortInstalled(state.Packages)
		if err := s.saveState(state); err != nil {
			_ = os.RemoveAll(target)
			cleanup()
			return results, err
		}
		cleanup()
		installedByName[want.Name] = installed
		results = append(results, SyncResult{Name: want.Name, Action: "installed", Installed: &installed})
	}
	return results, nil
}
