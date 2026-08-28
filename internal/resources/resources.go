// Package resources discovers and expands file-backed skills and prompt templates.
package resources

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/trobrock/notch/internal/extension"
	"github.com/trobrock/notch/internal/model"
)

var (
	// ErrSkillNotFound is returned when a /skill:name command names no loaded skill.
	ErrSkillNotFound = errors.New("skill not found")
	// ErrTemplateNotFound is returned when a slash command names no loaded template.
	ErrTemplateNotFound = errors.New("template not found")
	// ErrInvalidCommand is returned for an incomplete resource command.
	ErrInvalidCommand = errors.New("invalid resource command")
)

// Skill is a markdown skill definition.
type Skill struct {
	Name        string
	Description string
	Content     string
	Path        string
}

// Template is a markdown prompt template.
type Template struct {
	Name         string
	Description  string
	ArgumentHint string
	Content      string
	Path         string
}

// Catalog contains resources keyed by their command name.
type Catalog struct {
	Skills    map[string]Skill
	Templates map[string]Template
}

// Load discovers resources in the supplied directories. Directories are
// considered in order, so a resource in a later (typically project) directory
// replaces one with the same name from an earlier (typically global) directory.
// A missing directory is not an error.
func Load(skillDirs, promptDirs []string) (*Catalog, error) {
	catalog := &Catalog{
		Skills:    make(map[string]Skill),
		Templates: make(map[string]Template),
	}

	var loadErrors []error
	for _, dir := range skillDirs {
		paths, err := skillPaths(dir)
		if err != nil {
			loadErrors = append(loadErrors, err)
			continue
		}
		for _, path := range paths {
			fallback := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
			if strings.EqualFold(filepath.Base(path), "SKILL.md") {
				fallback = filepath.Base(filepath.Dir(path))
			}
			name, description, _, content, err := readMarkdown(path, fallback)
			if err != nil {
				loadErrors = append(loadErrors, fmt.Errorf("load skill %s: %w", path, err))
				continue
			}
			catalog.Skills[name] = Skill{Name: name, Description: description, Content: content, Path: path}
		}
	}

	for _, dir := range promptDirs {
		paths, err := templatePaths(dir)
		if err != nil {
			loadErrors = append(loadErrors, err)
			continue
		}
		for _, path := range paths {
			fallback := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
			name, description, argumentHint, content, err := readMarkdown(path, fallback)
			if err != nil {
				loadErrors = append(loadErrors, fmt.Errorf("load prompt template %s: %w", path, err))
				continue
			}
			catalog.Templates[name] = Template{Name: name, Description: description, ArgumentHint: argumentHint, Content: content, Path: path}
		}
	}

	return catalog, errors.Join(loadErrors...)
}

func skillPaths(dir string) ([]string, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("discover skills in %s: %w", dir, err)
	}
	var paths []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
			paths = append(paths, filepath.Join(dir, entry.Name()))
			continue
		}
		if entry.IsDir() {
			path := filepath.Join(dir, entry.Name(), "SKILL.md")
			info, statErr := os.Stat(path)
			if statErr == nil && !info.IsDir() {
				paths = append(paths, path)
			} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
				return nil, fmt.Errorf("discover skill %s: %w", path, statErr)
			}
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func templatePaths(dir string) ([]string, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("discover prompt templates in %s: %w", dir, err)
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
			paths = append(paths, filepath.Join(dir, entry.Name()))
		}
	}
	sort.Strings(paths)
	return paths, nil
}

// readMarkdown reads the small YAML-like front matter understood by Notch. It
// deliberately supports only a few scalar fields, avoiding a YAML dependency.
func readMarkdown(path, fallbackName string) (name, description, argumentHint, content string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", "", "", err
	}
	return parseMarkdown(data, fallbackName)
}

func parseMarkdown(data []byte, fallbackName string) (name, description, argumentHint, content string, err error) {
	name = fallbackName
	content = string(data)

	// Accept both Unix and Windows line endings while preserving body text.
	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	if !strings.HasPrefix(normalized, "---\n") {
		return name, "", "", content, nil
	}
	end := strings.Index(normalized[4:], "\n---")
	if end < 0 {
		return "", "", "", "", errors.New("unterminated front matter")
	}
	end += 4
	// The closing delimiter must occupy its complete line.
	afterDelimiter := normalized[end+4:]
	if afterDelimiter != "" && !strings.HasPrefix(afterDelimiter, "\n") {
		return "", "", "", "", errors.New("invalid front matter delimiter")
	}

	metadata := normalized[4:end]
	values := parseMetadata(metadata)
	if value := strings.TrimSpace(values["name"]); value != "" {
		name = value
	}
	description = strings.TrimSpace(values["description"])
	argumentHint = strings.TrimSpace(values["argument-hint"])
	if strings.TrimSpace(name) == "" {
		return "", "", "", "", errors.New("resource name is empty")
	}
	content = strings.TrimPrefix(afterDelimiter, "\n")
	return name, description, argumentHint, content, nil
}

func parseMetadata(metadata string) map[string]string {
	values := make(map[string]string)
	lines := strings.Split(metadata, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		key = strings.ToLower(strings.TrimSpace(key))
		if !ok || (key != "name" && key != "description" && key != "argument-hint") {
			continue
		}
		value = strings.TrimSpace(value)
		if value == "|" || value == ">" {
			fold := value == ">"
			var block []string
			for i+1 < len(lines) && (strings.HasPrefix(lines[i+1], " ") || strings.HasPrefix(lines[i+1], "\t") || strings.TrimSpace(lines[i+1]) == "") {
				i++
				block = append(block, strings.TrimSpace(lines[i]))
			}
			separator := "\n"
			if fold {
				separator = " "
			}
			value = strings.Join(block, separator)
		} else if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
			if value[0] == '"' {
				if unquoted, unquoteErr := strconv.Unquote(value); unquoteErr == nil {
					value = unquoted
				}
			} else {
				value = strings.ReplaceAll(value[1:len(value)-1], "''", "'")
			}
		}
		values[key] = value
	}
	return values
}

// SystemSummary describes the commands available in the catalog. Entries are
// sorted so the generated system prompt is stable.
func (c *Catalog) SystemSummary(skillToolAvailable ...bool) string {
	if c == nil {
		return ""
	}
	var sections []string
	if len(c.Skills) != 0 {
		names := sortedSkillNames(c.Skills)
		var b strings.Builder
		if len(skillToolAvailable) != 0 && skillToolAvailable[0] {
			b.WriteString("Available skills (load with the `skill` tool by name; `/skill:name` is a user command, not a file path):\n")
		} else {
			b.WriteString("Available user-invoked skills (`/skill:name` is a command, not a file path):\n")
		}
		for _, name := range names {
			fmt.Fprintf(&b, "- /skill:%s", name)
			if description := strings.TrimSpace(c.Skills[name].Description); description != "" {
				fmt.Fprintf(&b, ": %s", description)
			}
			b.WriteByte('\n')
		}
		sections = append(sections, strings.TrimSuffix(b.String(), "\n"))
	}
	if len(c.Templates) != 0 {
		names := sortedTemplateNames(c.Templates)
		var b strings.Builder
		b.WriteString("Available prompt templates:\n")
		for _, name := range names {
			fmt.Fprintf(&b, "- /%s", name)
			if description := strings.TrimSpace(c.Templates[name].Description); description != "" {
				fmt.Fprintf(&b, ": %s", description)
			}
			b.WriteByte('\n')
		}
		sections = append(sections, strings.TrimSuffix(b.String(), "\n"))
	}
	return strings.Join(sections, "\n\n")
}

// RegisterSkillTool lets the model load skill instructions without mistaking
// the user-facing /skill:name command for a filesystem path.
func (c *Catalog) RegisterSkillTool(registry *extension.Registry) (bool, error) {
	if c == nil || registry == nil || len(c.Skills) == 0 {
		return false, nil
	}
	if _, exists := registry.Tool("skill"); exists {
		return true, nil
	}
	names := sortedSkillNames(c.Skills)
	description := "Load the instructions for an available skill by name. Available skills: " + strings.Join(names, ", ") + ". Do not try to read /skill:name as a file path."
	err := registry.RegisterTool(extension.Tool{
		Definition: model.ToolDefinition{
			Name: "skill", Description: description,
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":      map[string]any{"type": "string", "enum": names, "description": "Skill name to load"},
					"arguments": map[string]any{"type": "string", "description": "Optional arguments substituted for $ARGUMENTS"},
				},
				"required": []string{"name"}, "additionalProperties": false,
			},
		},
		Source: "builtin:skills",
		Execute: func(_ context.Context, raw json.RawMessage, _ func(string)) (extension.ToolResult, error) {
			var input struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}
			if err := json.Unmarshal(raw, &input); err != nil {
				return extension.ToolResult{Content: "invalid skill input: " + err.Error(), IsError: true}, nil
			}
			skill, ok := c.lookupSkill(input.Name)
			if !ok {
				return extension.ToolResult{Content: "skill not found: " + input.Name, IsError: true}, nil
			}
			content := strings.ReplaceAll(skill.Content, "$ARGUMENTS", strings.TrimSpace(input.Arguments))
			return extension.ToolResult{Content: content, Details: map[string]any{"skill": skill.Name, "path": skill.Path}}, nil
		},
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

func (c *Catalog) lookupSkill(name string) (Skill, bool) {
	name = strings.TrimSpace(name)
	if c == nil || name == "" {
		return Skill{}, false
	}
	if skill, ok := c.Skills[name]; ok {
		return skill, true
	}
	for key, skill := range c.Skills {
		if strings.EqualFold(key, name) {
			return skill, true
		}
	}
	return Skill{}, false
}

// ExpandInput expands a leading resource slash command. Ordinary input is
// returned unchanged. The text following the command replaces every
// $ARGUMENTS marker in the selected markdown file.
func (c *Catalog) ExpandInput(input string) (string, error) {
	command, arguments := splitCommand(input)
	if !strings.HasPrefix(command, "/") {
		return input, nil
	}
	if strings.HasPrefix(command, "/skill:") {
		name := strings.TrimPrefix(command, "/skill:")
		if name == "" {
			return "", fmt.Errorf("%w: skill name is empty", ErrInvalidCommand)
		}
		if c == nil {
			return "", fmt.Errorf("%w: %s", ErrSkillNotFound, name)
		}
		skill, ok := c.lookupSkill(name)
		if !ok {
			return "", fmt.Errorf("%w: %s", ErrSkillNotFound, name)
		}
		return strings.ReplaceAll(skill.Content, "$ARGUMENTS", arguments), nil
	}

	name := strings.TrimPrefix(command, "/")
	if c != nil {
		if template, ok := c.Templates[name]; ok {
			return strings.ReplaceAll(template.Content, "$ARGUMENTS", arguments), nil
		}
	}
	if name == "" {
		return "", fmt.Errorf("%w: template name is empty", ErrInvalidCommand)
	}
	return "", fmt.Errorf("%w: %s", ErrTemplateNotFound, name)
}

func splitCommand(input string) (command, arguments string) {
	trimmed := strings.TrimLeft(input, " \t\r\n")
	if trimmed == "" {
		return "", ""
	}
	index := strings.IndexAny(trimmed, " \t\r\n")
	if index < 0 {
		return trimmed, ""
	}
	return trimmed[:index], strings.TrimSpace(trimmed[index:])
}

func sortedSkillNames(resources map[string]Skill) []string {
	names := make([]string, 0, len(resources))
	for name := range resources {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sortedTemplateNames(resources map[string]Template) []string {
	names := make([]string, 0, len(resources))
	for name := range resources {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
