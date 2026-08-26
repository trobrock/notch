package extpkg

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDesiredManifest(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "package")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "extensions.json")
	writeDesiredManifest(t, path, DesiredManifest{Version: 1, Packages: []DesiredPackage{
		{Name: "z-package", Source: "github:owner/z", Ref: "v1.0.0"},
		{Name: "local-package", Source: "./package"},
	}})
	desired, err := LoadDesiredManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(desired) != 2 || desired[0].Name != "local-package" || desired[0].Source.Location != source || desired[1].Source.Ref != "v1.0.0" {
		t.Fatalf("desired = %#v", desired)
	}
}

func TestLoadDesiredManifestRejectsInvalidInput(t *testing.T) {
	root := t.TempDir()
	tests := map[string]string{
		"unknown version": `{"version":2,"packages":[]}`,
		"duplicate name":  `{"version":1,"packages":[{"name":"demo","source":"github:a/b"},{"name":"demo","source":"github:a/c"}]}`,
		"unknown field":   `{"version":1,"packages":[],"extra":true}`,
		"trailing JSON":   `{"version":1,"packages":[]} {}`,
	}
	for name, contents := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(root, strings.ReplaceAll(name, " ", "-")+".json")
			if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadDesiredManifest(path); err == nil {
				t.Fatal("invalid manifest succeeded")
			}
		})
	}
}

func TestSyncMissingInstallsConvergesAndDryRuns(t *testing.T) {
	root, sources := t.TempDir(), t.TempDir()
	alpha, beta := filepath.Join(sources, "alpha"), filepath.Join(sources, "beta")
	if err := os.MkdirAll(alpha, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(beta, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestPackage(t, alpha, "alpha", "1.0.0", "alpha")
	writeTestPackage(t, beta, "beta", "2.0.0", "beta")
	store := New(root)
	desired := []Desired{{Name: "alpha", Source: Source{Type: "local", Location: alpha}}, {Name: "beta", Source: Source{Type: "local", Location: beta}}}

	dry, err := store.SyncMissing(context.Background(), desired, true)
	if err != nil || len(dry) != 2 || dry[0].Action != "install" || dry[1].Action != "install" {
		t.Fatalf("dry run = %#v, %v", dry, err)
	}
	statuses, err := store.List()
	if err != nil || len(statuses) != 0 {
		t.Fatalf("list after dry run = %#v, %v", statuses, err)
	}

	results, err := store.SyncMissing(context.Background(), desired, false)
	if err != nil || len(results) != 2 || results[0].Action != "installed" || results[1].Action != "installed" {
		t.Fatalf("sync = %#v, %v", results, err)
	}
	results, err = store.SyncMissing(context.Background(), desired, false)
	if err != nil || results[0].Action != "unchanged" || results[1].Action != "unchanged" {
		t.Fatalf("second sync = %#v, %v", results, err)
	}
}

func TestSyncMissingRejectsNameAndSourceMismatch(t *testing.T) {
	t.Run("declared name", func(t *testing.T) {
		source := t.TempDir()
		writeTestPackage(t, source, "actual", "1.0.0", "content")
		store := New(t.TempDir())
		_, err := store.SyncMissing(context.Background(), []Desired{{Name: "expected", Source: Source{Type: "local", Location: source}}}, false)
		if err == nil || !strings.Contains(err.Error(), `source declares package "actual"`) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("installed source", func(t *testing.T) {
		first, second := t.TempDir(), t.TempDir()
		writeTestPackage(t, first, "demo", "1.0.0", "one")
		writeTestPackage(t, second, "demo", "1.0.0", "two")
		store := New(t.TempDir())
		if _, err := store.Install(context.Background(), Source{Type: "local", Location: first}); err != nil {
			t.Fatal(err)
		}
		_, err := store.SyncMissing(context.Background(), []Desired{{Name: "demo", Source: Source{Type: "local", Location: second}}}, false)
		if err == nil || !strings.Contains(err.Error(), "installed from") {
			t.Fatalf("error = %v", err)
		}
	})
}

func writeDesiredManifest(t *testing.T, path string, manifest DesiredManifest) {
	t.Helper()
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
