package resources

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeResource(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadDiscoversMetadataAndOverrides(t *testing.T) {
	globalSkills := filepath.Join(t.TempDir(), "skills")
	projectSkills := filepath.Join(t.TempDir(), "skills")
	prompts := filepath.Join(t.TempDir(), "prompts")
	writeResource(t, filepath.Join(globalSkills, "review.md"), "---\nname: review\ndescription: global review\n---\nglobal $ARGUMENTS")
	writeResource(t, filepath.Join(globalSkills, "nested", "SKILL.md"), "---\ndescription: 'nested skill'\n---\nnested")
	writeResource(t, filepath.Join(globalSkills, "nested", "ignored.md"), "ignored")
	writeResource(t, filepath.Join(projectSkills, "replacement.md"), "---\nname: review\ndescription: project review\n---\nproject $ARGUMENTS")
	writeResource(t, filepath.Join(prompts, "fix.md"), "---\ndescription: Fix a bug\n---\nFix $ARGUMENTS")
	writeResource(t, filepath.Join(prompts, "ignore.txt"), "no")

	catalog, err := Load([]string{globalSkills, projectSkills}, []string{prompts})
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Skills) != 2 || catalog.Skills["nested"].Description != "nested skill" {
		t.Fatalf("skills = %#v", catalog.Skills)
	}
	if got := catalog.Skills["review"]; got.Content != "project $ARGUMENTS" || got.Description != "project review" {
		t.Fatalf("override = %#v", got)
	}
	if len(catalog.Templates) != 1 || catalog.Templates["fix"].Description != "Fix a bug" {
		t.Fatalf("templates = %#v", catalog.Templates)
	}
}

func TestExpandInput(t *testing.T) {
	catalog := &Catalog{
		Skills:    map[string]Skill{"review": {Content: "Review: $ARGUMENTS / $ARGUMENTS"}},
		Templates: map[string]Template{"fix": {Content: "Fix\n$ARGUMENTS"}},
	}
	got, err := catalog.ExpandInput("/skill:review this code")
	if err != nil || got != "Review: this code / this code" {
		t.Fatalf("skill expansion = %q, %v", got, err)
	}
	got, err = catalog.ExpandInput(" /fix issue 12\nwith care ")
	if err != nil || got != "Fix\nissue 12\nwith care" {
		t.Fatalf("template expansion = %q, %v", got, err)
	}
	got, err = catalog.ExpandInput("ordinary input")
	if err != nil || got != "ordinary input" {
		t.Fatalf("ordinary input = %q, %v", got, err)
	}
	if _, err := catalog.ExpandInput("/skill:missing"); !errors.Is(err, ErrSkillNotFound) {
		t.Fatalf("missing skill error = %v", err)
	}
	if _, err := catalog.ExpandInput("/missing"); !errors.Is(err, ErrTemplateNotFound) {
		t.Fatalf("missing template error = %v", err)
	}
}

func TestSystemSummaryIsSorted(t *testing.T) {
	catalog := &Catalog{
		Skills: map[string]Skill{
			"zeta":  {Description: "last"},
			"alpha": {Description: "first"},
		},
		Templates: map[string]Template{"repair": {Description: "repair it"}},
	}
	want := "Available skills:\n- /skill:alpha: first\n- /skill:zeta: last\n\nAvailable prompt templates:\n- /repair: repair it"
	if got := catalog.SystemSummary(); got != want {
		t.Fatalf("summary:\n%s\nwant:\n%s", got, want)
	}
}

func TestLoadReportsMalformedFrontMatterAndIgnoresMissingDirs(t *testing.T) {
	dir := t.TempDir()
	writeResource(t, filepath.Join(dir, "bad.md"), "---\nname: bad\nno close")
	catalog, err := Load([]string{filepath.Join(dir, "absent"), dir}, nil)
	if err == nil || !strings.Contains(err.Error(), "unterminated front matter") {
		t.Fatalf("error = %v", err)
	}
	if len(catalog.Skills) != 0 {
		t.Fatalf("skills = %#v", catalog.Skills)
	}
}
