package extpkg

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	invalidNameCharacter    = regexp.MustCompile(`[^a-z0-9._-]+`)
	invalidCommandCharacter = regexp.MustCompile(`[^a-z0-9_]+`)
)

// Validation describes a package which passed manifest, path, size, and tree
// integrity checks.
type Validation struct {
	Manifest  Manifest `json:"manifest"`
	Integrity string   `json:"integrity"`
}

// Validate checks a package directory without installing it.
func Validate(directory string) (Validation, error) {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return Validation{}, err
	}
	if err := validatePackageTree(absolute); err != nil {
		return Validation{}, fmt.Errorf("validate package files: %w", err)
	}
	manifest, err := loadManifest(absolute)
	if err != nil {
		return Validation{}, err
	}
	integrity, err := treeIntegrity(absolute)
	if err != nil {
		return Validation{}, fmt.Errorf("hash package: %w", err)
	}
	return Validation{Manifest: manifest, Integrity: integrity}, nil
}

// Init creates a minimal shareable Lua extension package without overwriting
// existing manifest or extension files.
func Init(directory, requestedName string) (Manifest, error) {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return Manifest{}, err
	}
	name := strings.ToLower(strings.TrimSpace(requestedName))
	if name == "" {
		name = strings.ToLower(filepath.Base(absolute))
		name = invalidNameCharacter.ReplaceAllString(name, "-")
		name = strings.Trim(name, ".-_")
	}
	if !packageNamePattern.MatchString(name) {
		return Manifest{}, fmt.Errorf("invalid package name %q", name)
	}
	if err := os.MkdirAll(absolute, 0o755); err != nil {
		return Manifest{}, err
	}
	manifestPath := filepath.Join(absolute, ManifestName)
	if _, err := os.Stat(manifestPath); err == nil {
		return Manifest{}, fmt.Errorf("package manifest already exists at %q", manifestPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Manifest{}, err
	}
	extensionsDir := filepath.Join(absolute, "extensions")
	if err := os.MkdirAll(extensionsDir, 0o755); err != nil {
		return Manifest{}, err
	}
	luaPath := filepath.Join(extensionsDir, "hello.lua")
	if _, err := os.Stat(luaPath); err == nil {
		return Manifest{}, fmt.Errorf("example extension already exists at %q", luaPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Manifest{}, err
	}
	manifest := Manifest{
		SchemaVersion: 1, Name: name, Version: "0.1.0",
		Description: "A Notch extension package", Extensions: []string{"extensions"},
	}
	contents, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return Manifest{}, err
	}
	contents = append(contents, '\n')
	if err := os.WriteFile(manifestPath, contents, 0o644); err != nil {
		return Manifest{}, err
	}
	commandName := invalidCommandCharacter.ReplaceAllString(name, "_") + "_hello"
	lua := fmt.Sprintf(`notch.register_command({
  name = %q,
  description = "Example command from the %s package",
  execute = function(args)
    if args == "" then
      return "hello from %s"
    end
    return "hello " .. args .. " from %s"
  end,
})
`, commandName, name, name, name)
	if err := os.WriteFile(luaPath, []byte(lua), 0o644); err != nil {
		_ = os.Remove(manifestPath)
		return Manifest{}, err
	}
	return manifest, nil
}
