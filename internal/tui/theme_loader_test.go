package tui

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadThemeCatalogCustomInheritanceVariablesAndRuntimeSwitch(t *testing.T) {
	dir := t.TempDir()
	writeThemeTestFile(t, filepath.Join(dir, "rose_pine.json"), `{
		"base": "dracula",
		"vars": {"foam":"#9ccfd8", "surface":"#26233a", "alias":"foam"},
		"colors": {
			"accent":"alias", "userMessageBg":"surface", "mdHeading":"#f6c177",
			"toolSuccessBg":"#283b35", "thinkingXhigh":"#eb6f92"
		}
	}`)
	catalog, warnings := LoadThemeCatalog(dir)
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	theme, canonical, ok := catalog.Lookup("Rose Pine")
	if !ok || canonical != "rose-pine" {
		t.Fatalf("lookup = %q, %v", canonical, ok)
	}
	if theme.Accent != "\x1b[38;2;156;207;216m" || theme.UserBG != "\x1b[48;2;38;35;58m" || theme.MarkdownHeading != "\x1b[38;2;246;193;119m" || theme.ThinkingXHigh != "\x1b[38;2;235;111;146m" {
		t.Fatalf("custom colors = %#v", theme)
	}
	dracula, _ := ThemeByName("dracula")
	if theme.Text != dracula.Text || theme.ToolErrorBG != dracula.ToolErrorBG {
		t.Fatal("unspecified colors did not inherit from base")
	}
	if theme.Link != theme.MarkdownLink || theme.User != theme.UserText {
		t.Fatal("legacy aliases were not synchronized")
	}
	wantNames := []string{"catppuccin-mocha", "dark", "dracula", "rose-pine"}
	if names := catalog.Names(); !reflect.DeepEqual(names, wantNames) {
		t.Fatalf("names = %#v, want %#v", names, wantNames)
	}

	app := NewApp(AppConfig{Themes: catalog})
	app.submit(testContext(), "/theme rose_pine")
	if app.state.layout.ThemeName != "rose-pine" || app.state.layout.Theme != theme {
		t.Fatalf("runtime theme = %q %#v", app.state.layout.ThemeName, app.state.layout.Theme)
	}
}

func TestLoadThemeCatalogDirectoryPrecedenceAndBuiltinOverride(t *testing.T) {
	user, project := t.TempDir(), t.TempDir()
	writeThemeTestFile(t, filepath.Join(user, "shared.json"), `{"name":"shared","colors":{"accent":"#111111"}}`)
	writeThemeTestFile(t, filepath.Join(project, "shared.json"), `{"name":"shared","colors":{"accent":"#222222"}}`)
	writeThemeTestFile(t, filepath.Join(project, "dark.json"), `{"name":"dark","colors":{"accent":"#abcdef"}}`)
	catalog, warnings := LoadThemeCatalog(user, project)
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	shared, _, _ := catalog.Lookup("shared")
	if shared.Accent != "\x1b[38;2;34;34;34m" {
		t.Fatalf("later directory did not win: %q", shared.Accent)
	}
	dark, canonical, ok := catalog.Lookup("pi-dark")
	if !ok || canonical != "dark" || dark.Accent != "\x1b[38;2;171;205;239m" {
		t.Fatalf("built-in override = %q, %v, %q", canonical, ok, dark.Accent)
	}
}

func TestLoadThemeCatalogAcceptsPiShapedTheme(t *testing.T) {
	dir := t.TempDir()
	writeThemeTestFile(t, filepath.Join(dir, "pi.json"), `{
		"$schema":"https://example.test/theme.json", "name":"pi-shaped",
		"vars":{"text":"#d4d4d4", "gray":"#808080", "surface":"#343541"},
		"colors":{
			"text":"text", "muted":"gray", "userMessageBg":"surface",
			"mdCodeBlockBorder":"gray", "mdQuoteBorder":"gray",
			"selectedBg":"surface", "syntaxKeyword":"#569cd6", "thinkingMax":"#ff5fff"
		},
		"export":{"pageBg":"#18181e"}
	}`)
	catalog, warnings := LoadThemeCatalog(dir)
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	theme, _, ok := catalog.Lookup("pi-shaped")
	if !ok || theme.Text != "\x1b[38;2;212;212;212m" || theme.UserBG != "\x1b[48;2;52;53;65m" || theme.MarkdownQuote != "\x1b[38;2;128;128;128m" {
		t.Fatalf("Pi-shaped theme = %#v, %v", theme, ok)
	}
}

func TestLoadThemeCatalogWarnsAndSkipsInvalidThemes(t *testing.T) {
	dir := t.TempDir()
	writeThemeTestFile(t, filepath.Join(dir, "bad-json.json"), `{bad`)
	writeThemeTestFile(t, filepath.Join(dir, "bad-name.json"), `{"name":"../bad","colors":{}}`)
	writeThemeTestFile(t, filepath.Join(dir, "bad-color.json"), `{"colors":{"accent":"red"}}`)
	writeThemeTestFile(t, filepath.Join(dir, "bad-role.json"), `{"colors":{"typoColor":"#ffffff"}}`)
	writeThemeTestFile(t, filepath.Join(dir, "cycle-a.json"), `{"name":"cycle-a","base":"cycle-b"}`)
	writeThemeTestFile(t, filepath.Join(dir, "cycle-b.json"), `{"name":"cycle-b","base":"cycle-a"}`)
	catalog, warnings := LoadThemeCatalog(filepath.Join(dir, "missing"), dir)
	if len(warnings) != 6 {
		t.Fatalf("warnings = %d: %v", len(warnings), warnings)
	}
	joined := make([]string, len(warnings))
	for i, warning := range warnings {
		joined[i] = warning.Error()
	}
	text := strings.Join(joined, "\n")
	for _, want := range []string{"bad-json.json", "invalid name", "expected #RRGGBB", "unknown color role", "inheritance cycle"} {
		if !strings.Contains(text, want) {
			t.Errorf("warnings missing %q: %s", want, text)
		}
	}
	for _, name := range []string{"bad-json", "bad-name", "bad-color", "bad-role", "cycle-a", "cycle-b"} {
		if _, _, ok := catalog.Lookup(name); ok {
			t.Errorf("invalid theme %q loaded", name)
		}
	}
}

func writeThemeTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func testContext() context.Context { return context.Background() }
