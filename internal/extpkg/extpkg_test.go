package extpkg

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestParseSource(t *testing.T) {
	cwd := t.TempDir()
	local := filepath.Join(cwd, "local")
	if err := os.Mkdir(local, 0o755); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		raw, ref, subdir string
		want             Source
	}{
		{"github:owner/repo", "", "", Source{Type: "github", Location: "owner/repo"}},
		{"github:owner/repo@v1.2.0//packages/demo", "", "", Source{Type: "github", Location: "owner/repo", Ref: "v1.2.0", Subdir: filepath.Join("packages", "demo")}},
		{"https://github.com/owner/repo.git#main", "", "pkg", Source{Type: "github", Location: "owner/repo", Ref: "main", Subdir: "pkg"}},
		{"git:https://gitlab.example/owner/repo.git#stable", "", "", Source{Type: "git", Location: "https://gitlab.example/owner/repo.git", Ref: "stable"}},
		{"./local", "", "", Source{Type: "local", Location: local}},
	}
	for _, test := range tests {
		got, err := ParseSource(test.raw, cwd, test.ref, test.subdir)
		if err != nil || !reflect.DeepEqual(got, test.want) {
			t.Errorf("ParseSource(%q) = %#v, %v; want %#v", test.raw, got, err, test.want)
		}
	}
	for _, invalid := range []string{"", "github:owner", "https://github.com/owner/repo/tree/main", "git:https://user:secret@example.com/repo", "missing-relative-path"} {
		if _, err := ParseSource(invalid, cwd, "", ""); err == nil {
			t.Errorf("ParseSource(%q) succeeded", invalid)
		}
	}
	if _, err := ParseSource("github:owner/repo@main", cwd, "other", ""); err == nil {
		t.Fatal("conflicting refs succeeded")
	}
	if _, err := ParseSource("github:owner/repo", cwd, "", "../escape"); err == nil {
		t.Fatal("escaping subdirectory succeeded")
	}
}

func TestLocalInstallUpdateListDiscoveryAndRemove(t *testing.T) {
	root, source := t.TempDir(), t.TempDir()
	writeTestPackage(t, source, "demo", "1.0.0", "one")
	now := time.Date(2026, 8, 24, 1, 2, 3, 0, time.UTC)
	store := NewWithOptions(Options{Root: root, Now: func() time.Time { return now }})
	parsed, err := ParseSource(source, t.TempDir(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	installed, err := store.Install(context.Background(), parsed)
	if err != nil {
		t.Fatal(err)
	}
	if installed.Name != "demo" || installed.Version != "1.0.0" || installed.Source.Type != "local" || !strings.HasPrefix(installed.Integrity, "sha256:") || installed.Resolved != installed.Integrity {
		t.Fatalf("installed = %#v", installed)
	}
	if _, err := store.Install(context.Background(), parsed); err == nil {
		t.Fatal("duplicate install succeeded")
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(filepath.Join(root, "packages.json")); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("state mode = %v, %v", info, err)
		}
	}
	dirs, err := DiscoveryDirs(root)
	wantDir := filepath.Join(root, "packages", "demo", "extensions")
	if err != nil || !reflect.DeepEqual(dirs, []string{wantDir}) {
		t.Fatalf("discovery dirs = %#v, %v", dirs, err)
	}
	statuses, err := store.List()
	if err != nil || len(statuses) != 1 || statuses[0].State != "ok" {
		t.Fatalf("statuses = %#v, %v", statuses, err)
	}
	if err := os.WriteFile(filepath.Join(wantDir, "demo.lua"), []byte("modified"), 0o644); err != nil {
		t.Fatal(err)
	}
	statuses, _ = store.List()
	if statuses[0].State != "modified" {
		t.Fatalf("modified status = %#v", statuses[0])
	}

	now = now.Add(time.Hour)
	writeTestPackage(t, source, "demo", "1.1.0", "two")
	results, err := store.Update(context.Background(), []string{"demo"}, false)
	if err != nil || len(results) != 1 || !results[0].Changed || results[0].After.Version != "1.1.0" {
		t.Fatalf("update = %#v, %v", results, err)
	}
	contents, err := os.ReadFile(filepath.Join(wantDir, "demo.lua"))
	if err != nil || string(contents) != "two" {
		t.Fatalf("updated extension = %q, %v", contents, err)
	}
	results, err = store.Update(context.Background(), nil, false)
	if err != nil || len(results) != 1 || results[0].Changed {
		t.Fatalf("no-op update = %#v, %v", results, err)
	}
	if err := os.WriteFile(filepath.Join(wantDir, "demo.lua"), []byte("locally modified"), 0o644); err != nil {
		t.Fatal(err)
	}
	results, err = store.Update(context.Background(), nil, false)
	if err != nil || len(results) != 1 || !results[0].Changed {
		t.Fatalf("reconciliation update = %#v, %v", results, err)
	}
	if contents, err := os.ReadFile(filepath.Join(wantDir, "demo.lua")); err != nil || string(contents) != "two" {
		t.Fatalf("reconciled extension = %q, %v", contents, err)
	}
	if err := os.RemoveAll(filepath.Join(root, "packages", "demo")); err != nil {
		t.Fatal(err)
	}
	results, err = store.Update(context.Background(), nil, false)
	if err != nil || len(results) != 1 || !results[0].Changed {
		t.Fatalf("missing package repair = %#v, %v", results, err)
	}

	writeTestPackage(t, source, "demo", "0.9.0", "old")
	if _, err := store.Update(context.Background(), nil, false); err == nil || !strings.Contains(err.Error(), "downgrade") {
		t.Fatalf("downgrade error = %v", err)
	}
	if _, err := store.Update(context.Background(), nil, true); err != nil {
		t.Fatalf("forced downgrade: %v", err)
	}
	removed, err := store.Remove("demo")
	if err != nil || removed.Name != "demo" {
		t.Fatalf("removed = %#v, %v", removed, err)
	}
	statuses, err = store.List()
	if err != nil || len(statuses) != 0 {
		t.Fatalf("statuses after remove = %#v, %v", statuses, err)
	}
	if dirs, err := DiscoveryDirs(root); err != nil || len(dirs) != 0 {
		t.Fatalf("discovery after remove = %#v, %v", dirs, err)
	}
}

func TestGenericGitInstallAtCommit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file URL setup differs on Windows")
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	repository := t.TempDir()
	writeTestPackage(t, repository, "git-demo", "1.0.0", "git package")
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"add", "."},
		{"commit", "--quiet", "-m", "package"},
	} {
		command := exec.Command(gitPath, args...)
		command.Dir = repository
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	command := exec.Command(gitPath, "rev-parse", "HEAD")
	command.Dir = repository
	shaBytes, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.TrimSpace(string(shaBytes))
	store := NewWithOptions(Options{Root: t.TempDir(), GitPath: gitPath})
	installed, err := store.Install(context.Background(), Source{Type: "git", Location: "file://" + filepath.ToSlash(repository), Ref: sha})
	if err != nil {
		t.Fatal(err)
	}
	if installed.Name != "git-demo" || installed.Resolved != sha {
		t.Fatalf("installed = %#v", installed)
	}
}

func TestDiscoveryRecoversInterruptedUpdate(t *testing.T) {
	root, source := t.TempDir(), t.TempDir()
	writeTestPackage(t, source, "recover-demo", "1.0.0", "ready")
	store := New(root)
	parsed, err := ParseSource(source, t.TempDir(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Install(context.Background(), parsed); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(store.packages, "recover-demo")
	backup := target + ".backup"
	if err := os.Rename(target, backup); err != nil {
		t.Fatal(err)
	}
	dirs, err := DiscoveryDirs(root)
	if err != nil || len(dirs) != 1 {
		t.Fatalf("discovery = %#v, %v", dirs, err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("target was not recovered: %v", err)
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Fatalf("backup still exists: %v", err)
	}

	// Simulate a crash after new content replaced the target but before the
	// old lock record was saved. Recovery must restore the matching backup.
	if err := os.Rename(target, backup); err != nil {
		t.Fatal(err)
	}
	writeTestPackage(t, source, "recover-demo", "2.0.0", "new but unlocked")
	if err := copyTree(source, target); err != nil {
		t.Fatal(err)
	}
	if _, err := DiscoveryDirs(root); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(filepath.Join(target, "extensions", "recover-demo.lua"))
	if err != nil || string(contents) != "ready" {
		t.Fatalf("recovered content = %q, %v", contents, err)
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Fatalf("rollback backup still exists: %v", err)
	}
}

func TestGitHubInstallAtResolvedCommitAndSubdir(t *testing.T) {
	archive := testArchive(t, map[string]string{
		"repository-deadbeef/packages/demo/notch-package.json":  manifestJSON("demo", "2.0.0"),
		"repository-deadbeef/packages/demo/extensions/demo.lua": "return true",
		"repository-deadbeef/unrelated.txt":                     "ignored",
	})
	const sha = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/owner/repo/commits/v2":
			_ = json.NewEncoder(w).Encode(map[string]string{"sha": sha})
		case "/repos/owner/repo/tarball/" + sha:
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	store := NewWithOptions(Options{Root: t.TempDir(), Client: server.Client(), GitHubAPIBase: server.URL})
	installed, err := store.Install(context.Background(), Source{
		Type: "github", Location: "owner/repo", Ref: "v2", Subdir: filepath.Join("packages", "demo"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if installed.Name != "demo" || installed.Version != "2.0.0" || installed.Resolved != sha {
		t.Fatalf("installed = %#v", installed)
	}
	if _, err := os.Stat(filepath.Join(store.packages, "demo", "unrelated.txt")); !os.IsNotExist(err) {
		t.Fatalf("unrelated repository content was installed: %v", err)
	}
}

func TestSemanticVersionComparison(t *testing.T) {
	for _, test := range []struct {
		left, right string
		want        int
	}{
		{"1.1.0", "1.0.9", 1},
		{"1.0.0-alpha.2", "1.0.0-alpha.10", -1},
		{"1.0.0", "1.0.0-rc.1", 1},
		{"v2.0.0+build.1", "2.0.0+build.2", 0},
	} {
		got := compareVersions(test.left, test.right)
		if (got < 0 && test.want >= 0) || (got > 0 && test.want <= 0) || (got == 0 && test.want != 0) {
			t.Errorf("compareVersions(%q, %q) = %d, want sign %d", test.left, test.right, got, test.want)
		}
	}
	for _, invalid := range []string{"1.0", "01.0.0", "1.0.0-01", "1.0.0-..", "1.0.0+.."} {
		if _, ok := parsePackageVersion(invalid); ok {
			t.Errorf("invalid version %q parsed", invalid)
		}
	}
}

func TestArchiveAndManifestRejectUnsafeContent(t *testing.T) {
	archive := testArchive(t, map[string]string{"root/../../escape": "bad"})
	if err := extractGitHubTarGzip(archive, filepath.Join(t.TempDir(), "out")); err == nil {
		t.Fatal("archive traversal succeeded")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ManifestName), []byte(`{"schema_version":1,"name":"demo","version":"1.0.0","extensions":["../outside"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadManifest(dir); err == nil {
		t.Fatal("escaping manifest path succeeded")
	}
}

func TestInitCreatesInstallablePackage(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "My Extension")
	manifest, err := Init(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Name != "my-extension" || manifest.Version != "0.1.0" {
		t.Fatalf("manifest = %#v", manifest)
	}
	loaded, err := loadManifest(dir)
	if err != nil || loaded.Name != manifest.Name {
		t.Fatalf("loaded = %#v, %v", loaded, err)
	}
	validation, err := Validate(dir)
	if err != nil || validation.Manifest.Name != manifest.Name || !integrityPattern.MatchString(validation.Integrity) {
		t.Fatalf("validation = %#v, %v", validation, err)
	}
	if _, err := Init(dir, ""); err == nil {
		t.Fatal("second init succeeded")
	}
}

func writeTestPackage(t *testing.T, dir, name, version, lua string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "extensions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestName), []byte(manifestJSON(name, version)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "extensions", name+".lua"), []byte(lua), 0o644); err != nil {
		t.Fatal(err)
	}
}

func manifestJSON(name, version string) string {
	return fmt.Sprintf(`{"schema_version":1,"name":%q,"version":%q,"description":"test package","extensions":["extensions"]}`, name, version)
}

func testArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var output bytes.Buffer
	gzipWriter := gzip.NewWriter(&output)
	tarWriter := tar.NewWriter(gzipWriter)
	for name, contents := range files {
		if err := tarWriter.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(contents)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write([]byte(contents)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}
