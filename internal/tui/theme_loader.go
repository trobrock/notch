package tui

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const maxThemeFileSize = 1 << 20

// ThemeCatalog contains built-in themes plus custom themes loaded for one
// Notch process. It is immutable after construction and safe for concurrent use.
type ThemeCatalog struct {
	themes   map[string]Theme
	builtins map[string]Theme
}

type themeFile struct {
	Name   string            `json:"name"`
	Base   string            `json:"base"`
	Vars   map[string]string `json:"vars"`
	Colors map[string]string `json:"colors"`
	source string
}

// BuiltinThemeCatalog returns an independent catalog containing built-ins.
func BuiltinThemeCatalog() *ThemeCatalog {
	builtins := make(map[string]Theme, len(builtinThemes))
	themes := make(map[string]Theme, len(builtinThemes))
	for name, theme := range builtinThemes {
		builtins[name], themes[name] = theme, theme
	}
	return &ThemeCatalog{themes: themes, builtins: builtins}
}

// LoadThemeCatalog loads direct JSON children of dirs in order. Later files
// override earlier themes with the same normalized name. Missing directories
// are ignored; malformed files are returned as warnings and skipped.
func LoadThemeCatalog(dirs ...string) (*ThemeCatalog, []error) {
	catalog := BuiltinThemeCatalog()
	definitions := make(map[string]themeFile)
	var warnings []error
	for _, dir := range dirs {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			warnings = append(warnings, fmt.Errorf("read theme directory %q: %w", dir, err))
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".json") {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			definition, err := readThemeFile(path)
			if err != nil {
				warnings = append(warnings, err)
				continue
			}
			definitions[normalizeThemeName(definition.Name)] = definition
		}
	}

	resolved := make(map[string]Theme, len(definitions))
	resolving := make(map[string]bool, len(definitions))
	var resolve func(string) (Theme, error)
	resolve = func(name string) (Theme, error) {
		name = normalizeThemeName(name)
		if theme, ok := resolved[name]; ok {
			return theme, nil
		}
		definition, custom := definitions[name]
		if !custom {
			if theme, _, ok := catalog.Lookup(name); ok {
				return theme, nil
			}
			return Theme{}, fmt.Errorf("unknown base theme %q", name)
		}
		if resolving[name] {
			return Theme{}, fmt.Errorf("theme inheritance cycle at %q", name)
		}
		resolving[name] = true
		defer delete(resolving, name)

		baseName := normalizeThemeName(definition.Base)
		if baseName == "" || baseName == "default" || baseName == "pi" || baseName == "pi-dark" {
			baseName = "dark"
		}
		var base Theme
		var err error
		if baseName == name {
			var ok bool
			base, ok = catalog.builtins[name]
			if !ok {
				return Theme{}, fmt.Errorf("theme %q cannot inherit from itself", name)
			}
		} else {
			base, err = resolve(baseName)
			if err != nil {
				return Theme{}, err
			}
		}
		theme, err := applyThemeColors(base, definition)
		if err != nil {
			return Theme{}, err
		}
		theme = syncThemeAliases(theme)
		resolved[name] = theme
		return theme, nil
	}

	names := make([]string, 0, len(definitions))
	for name := range definitions {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		theme, err := resolve(name)
		if err != nil {
			warnings = append(warnings, fmt.Errorf("load theme %q from %q: %w", name, definitions[name].source, err))
			continue
		}
		catalog.themes[name] = theme
	}
	return catalog, warnings
}

func readThemeFile(path string) (themeFile, error) {
	info, err := os.Stat(path)
	if err != nil {
		return themeFile{}, fmt.Errorf("stat theme %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return themeFile{}, fmt.Errorf("theme %q is not a regular file", path)
	}
	if info.Size() > maxThemeFileSize {
		return themeFile{}, fmt.Errorf("theme %q exceeds 1 MiB", path)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return themeFile{}, fmt.Errorf("read theme %q: %w", path, err)
	}
	var definition themeFile
	if err := json.Unmarshal(contents, &definition); err != nil {
		return themeFile{}, fmt.Errorf("parse theme %q: %w", path, err)
	}
	if strings.TrimSpace(definition.Name) == "" {
		definition.Name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	definition.Name = normalizeThemeName(definition.Name)
	if !validCustomThemeName(definition.Name) {
		return themeFile{}, fmt.Errorf("theme %q has invalid name %q (use letters, numbers, and hyphens)", path, definition.Name)
	}
	if definition.Colors == nil {
		definition.Colors = map[string]string{}
	}
	definition.source = path
	return definition, nil
}

// Lookup returns a theme and its canonical name. Exact custom names take
// precedence over aliases retained for built-in compatibility.
func (c *ThemeCatalog) Lookup(name string) (Theme, string, bool) {
	if c == nil {
		c = BuiltinThemeCatalog()
	}
	key := normalizeThemeName(name)
	if theme, ok := c.themes[key]; ok {
		return theme, key, true
	}
	switch key {
	case "", "default", "pi", "pi-dark":
		key = "dark"
	case "catppuccin", "mocha", "catppuccinmocha", "ctp-mocha", "ctpmocha":
		key = "catppuccin-mocha"
	}
	theme, ok := c.themes[key]
	return theme, key, ok
}

// Names returns canonical theme names in stable alphabetical order.
func (c *ThemeCatalog) Names() []string {
	if c == nil {
		c = BuiltinThemeCatalog()
	}
	names := make([]string, 0, len(c.themes))
	for name := range c.themes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func validCustomThemeName(name string) bool {
	if name == "" || strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		return false
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

func applyThemeColors(theme Theme, definition themeFile) (Theme, error) {
	keys := make([]string, 0, len(definition.Colors))
	for key := range definition.Colors {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		role := normalizeColorRole(key)
		if ignoredThemeRole(role) {
			continue
		}
		background, known := themeRoleKind(role)
		if !known {
			return Theme{}, fmt.Errorf("unknown color role %q", key)
		}
		color, err := resolveThemeColor(definition.Colors[key], definition.Vars, nil)
		if err != nil {
			return Theme{}, fmt.Errorf("color %q: %w", key, err)
		}
		style := fg(color)
		if background {
			style = bg(color)
		}
		setThemeRole(&theme, role, style)
	}
	return theme, nil
}

func resolveThemeColor(value string, vars map[string]string, stack map[string]bool) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) == 7 && value[0] == '#' {
		if _, err := strconv.ParseUint(value[1:], 16, 24); err == nil {
			return strings.ToLower(value), nil
		}
		return "", fmt.Errorf("invalid hex color %q", value)
	}
	replacement, ok := vars[value]
	if !ok {
		return "", fmt.Errorf("expected #RRGGBB or variable, got %q", value)
	}
	if stack == nil {
		stack = make(map[string]bool)
	}
	if stack[value] {
		return "", fmt.Errorf("variable cycle at %q", value)
	}
	stack[value] = true
	resolved, err := resolveThemeColor(replacement, vars, stack)
	delete(stack, value)
	return resolved, err
}

func normalizeColorRole(role string) string {
	role = strings.ToLower(strings.TrimSpace(role))
	return strings.NewReplacer("_", "", "-", "", " ", "").Replace(role)
}

func themeRoleKind(role string) (background bool, known bool) {
	switch role {
	case "usermessagebg", "userbg", "toolpendingbg", "toolsuccessbg", "toolerrorbg":
		return true, true
	case "text", "muted", "accent", "border", "mdheading", "mdlink", "mdlinkurl", "mdurl",
		"mdcode", "mdcodeblock", "mdcodeblockborder", "mdquote", "mdquoteborder", "mdhr", "mdrule", "mdlistbullet", "mdbullet",
		"usermessagetext", "usertext", "tooltitle", "tooloutput", "notice", "warning", "error",
		"composer", "footer", "dim", "thinkingoff", "thinkingminimal", "thinkinglow", "thinkingmedium",
		"thinkinghigh", "thinkingxhigh":
		return false, true
	default:
		return false, false
	}
}

func setThemeRole(theme *Theme, role, style string) {
	switch role {
	case "text":
		theme.Text = style
	case "muted":
		theme.Muted = style
	case "accent":
		theme.Accent = style
	case "border":
		theme.Border = style
	case "mdheading":
		theme.MarkdownHeading = style
	case "mdlink":
		theme.MarkdownLink = style
	case "mdlinkurl", "mdurl":
		theme.MarkdownURL = style
	case "mdcode":
		theme.MarkdownCode = style
	case "mdcodeblock":
		theme.MarkdownCodeBlock = style
	case "mdcodeblockborder", "mdquote", "mdquoteborder":
		theme.MarkdownQuote = style
	case "mdhr", "mdrule":
		theme.MarkdownRule = style
	case "mdlistbullet", "mdbullet":
		theme.MarkdownBullet = style
	case "usermessagebg", "userbg":
		theme.UserBG = style
	case "usermessagetext", "usertext":
		theme.UserText = style
	case "toolpendingbg":
		theme.ToolPendingBG = style
	case "toolsuccessbg":
		theme.ToolSuccessBG = style
	case "toolerrorbg":
		theme.ToolErrorBG = style
	case "tooltitle":
		theme.ToolTitle = style
	case "tooloutput":
		theme.ToolOutput = style
	case "notice", "warning":
		theme.Notice = style
	case "error":
		theme.Error = style
	case "composer":
		theme.Composer = style
	case "footer", "dim":
		theme.Footer = style
	case "thinkingoff":
		theme.ThinkingOff = style
	case "thinkingminimal":
		theme.ThinkingMinimal = style
	case "thinkinglow":
		theme.ThinkingLow = style
	case "thinkingmedium":
		theme.ThinkingMedium = style
	case "thinkinghigh":
		theme.ThinkingHigh = style
	case "thinkingxhigh":
		theme.ThinkingXHigh = style
	}
}

func ignoredThemeRole(role string) bool {
	switch role {
	case "borderaccent", "bordermuted", "success", "thinkingtext", "selectedbg", "scrollbarthumb",
		"searchmatchbg", "searchmatchtext", "custommessagebg", "custommessagetext", "custommessagelabel",
		"tooldiffadded", "tooldiffremoved", "tooldiffcontext", "syntaxcomment", "syntaxkeyword",
		"syntaxfunction", "syntaxvariable", "syntaxstring", "syntaxnumber", "syntaxtype", "syntaxoperator",
		"syntaxpunctuation", "thinkingmax", "bashmode":
		return true
	default:
		return false
	}
}
