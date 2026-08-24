// Package extpkg installs and tracks decentralized Notch extension packages.
package extpkg

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	ManifestName    = "notch-package.json"
	stateVersion    = 1
	maxManifestSize = 1 << 20
)

var (
	packageNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	integrityPattern   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	versionPattern     = regexp.MustCompile(`^v?(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)
)

// Manifest describes the extension directories exported by a package.
type Manifest struct {
	SchemaVersion int      `json:"schema_version"`
	Name          string   `json:"name"`
	Version       string   `json:"version"`
	Description   string   `json:"description,omitempty"`
	License       string   `json:"license,omitempty"`
	Homepage      string   `json:"homepage,omitempty"`
	Extensions    []string `json:"extensions"`
}

// Source identifies where an installed package can be fetched again.
type Source struct {
	Type     string `json:"type"`
	Location string `json:"location"`
	Ref      string `json:"ref,omitempty"`
	Subdir   string `json:"subdir,omitempty"`
}

func (s Source) String() string {
	value := s.Location
	switch s.Type {
	case "github":
		value = "github:" + value
		if s.Ref != "" {
			value += "@" + s.Ref
		}
		if s.Subdir != "" {
			value += "//" + filepath.ToSlash(s.Subdir)
		}
		return value
	case "git":
		value = "git:" + value
		if s.Ref != "" {
			value += "#" + s.Ref
		}
	case "local":
		value = "local:" + value
	}
	if s.Subdir != "" {
		value += " (subdir " + filepath.ToSlash(s.Subdir) + ")"
	}
	return value
}

// Installed is the durable lock record for one installed package.
type Installed struct {
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	Description string    `json:"description,omitempty"`
	Source      Source    `json:"source"`
	Resolved    string    `json:"resolved"`
	Integrity   string    `json:"integrity"`
	InstalledAt time.Time `json:"installed_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Status combines a lock record with a lightweight integrity state.
type Status struct {
	Installed
	State string `json:"state"`
}

type stateFile struct {
	Version  int         `json:"version"`
	Packages []Installed `json:"packages"`
}

func loadManifest(dir string) (Manifest, error) {
	path := filepath.Join(dir, ManifestName)
	f, err := os.Open(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("open package manifest %q: %w", path, err)
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil {
		return Manifest{}, fmt.Errorf("inspect package manifest %q: %w", path, err)
	} else if info.Size() > maxManifestSize {
		return Manifest{}, fmt.Errorf("package manifest %q exceeds %d bytes", path, maxManifestSize)
	}
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode package manifest %q: %w", path, err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return Manifest{}, fmt.Errorf("decode package manifest %q: %w", path, err)
	}
	if err := validateManifest(dir, manifest); err != nil {
		return Manifest{}, fmt.Errorf("validate package manifest %q: %w", path, err)
	}
	return manifest, nil
}

func validateManifest(dir string, manifest Manifest) error {
	if manifest.SchemaVersion != 1 {
		return fmt.Errorf("unsupported schema_version %d", manifest.SchemaVersion)
	}
	if len(manifest.Name) > 128 || !packageNamePattern.MatchString(manifest.Name) {
		return fmt.Errorf("invalid package name %q (use at most 128 lowercase letters, digits, '.', '_', or '-')", manifest.Name)
	}
	if len(manifest.Version) > 128 || !versionPattern.MatchString(manifest.Version) {
		return fmt.Errorf("invalid semantic version %q", manifest.Version)
	}
	if _, ok := parsePackageVersion(manifest.Version); !ok {
		return fmt.Errorf("invalid semantic version %q", manifest.Version)
	}
	if len(manifest.Description) > 4096 || len(manifest.License) > 128 || len(manifest.Homepage) > 2048 {
		return errors.New("package metadata field is too long")
	}
	if len(manifest.Extensions) == 0 {
		return errors.New("extensions must contain at least one directory")
	}
	if len(manifest.Extensions) > 64 {
		return errors.New("extensions contains too many directories")
	}
	seen := make(map[string]bool, len(manifest.Extensions))
	for _, extensionDir := range manifest.Extensions {
		if len(extensionDir) > 512 {
			return errors.New("extension directory path is too long")
		}
		if strings.Contains(extensionDir, "\\") {
			return errors.New("extension directory paths must use forward slashes")
		}
		clean, err := cleanRelativePath(extensionDir, true)
		if err != nil {
			return fmt.Errorf("extension directory %q: %w", extensionDir, err)
		}
		if filepath.ToSlash(clean) != extensionDir {
			return fmt.Errorf("extension directory %q is not a clean relative path", extensionDir)
		}
		if seen[clean] {
			return fmt.Errorf("duplicate extension directory %q", extensionDir)
		}
		seen[clean] = true
		info, err := os.Stat(filepath.Join(dir, clean))
		if err != nil {
			return fmt.Errorf("extension directory %q: %w", extensionDir, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("extension path %q is not a directory", extensionDir)
		}
	}
	return nil
}

func cleanRelativePath(value string, allowDot bool) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || filepath.IsAbs(value) {
		return "", errors.New("path must be non-empty and relative")
	}
	clean := filepath.Clean(filepath.FromSlash(value))
	if clean == "." && !allowDot {
		return "", errors.New("path must name a subdirectory")
	}
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("path escapes the package root")
	}
	return clean, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return errors.New("multiple JSON values")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func sortInstalled(packages []Installed) {
	sort.Slice(packages, func(i, j int) bool { return packages[i].Name < packages[j].Name })
}
