package resources

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
)

// bundledFS keeps Notch's self-configuration and extension-authoring guidance
// available in installed binaries without requiring source or documentation files.
//
//go:embed builtin/*/SKILL.md
var bundledFS embed.FS

// LoadBundled loads built-in skills first, then overlays configured user and
// project resources. A disk skill with the same declared name intentionally wins.
func LoadBundled(skillDirs, promptDirs []string) (*Catalog, error) {
	catalog, bundledErr := bundledCatalog()
	disk, diskErr := Load(skillDirs, promptDirs)
	if disk != nil {
		for name, skill := range disk.Skills {
			catalog.Skills[name] = skill
		}
		for name, template := range disk.Templates {
			catalog.Templates[name] = template
		}
	}
	return catalog, errors.Join(bundledErr, diskErr)
}

func bundledCatalog() (*Catalog, error) {
	catalog := &Catalog{Skills: make(map[string]Skill), Templates: make(map[string]Template)}
	paths, err := fs.Glob(bundledFS, "builtin/*/SKILL.md")
	if err != nil {
		return catalog, fmt.Errorf("discover bundled skills: %w", err)
	}
	sort.Strings(paths)
	for _, resourcePath := range paths {
		data, readErr := bundledFS.ReadFile(resourcePath)
		if readErr != nil {
			return catalog, fmt.Errorf("read bundled skill %s: %w", resourcePath, readErr)
		}
		fallback := path.Base(path.Dir(resourcePath))
		name, description, _, content, parseErr := parseMarkdown(data, fallback)
		if parseErr != nil {
			return catalog, fmt.Errorf("load bundled skill %s: %w", resourcePath, parseErr)
		}
		catalog.Skills[name] = Skill{Name: name, Description: description, Content: content, Path: "builtin:" + resourcePath}
	}
	return catalog, nil
}
