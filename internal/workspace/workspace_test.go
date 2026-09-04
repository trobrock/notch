package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRootCanonicalizesGitWorkspace(t *testing.T) {
	root := t.TempDir()
	if err := exec.Command("git", "init", "-q", root).Run(); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	nested := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := Root(nested)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("root = %q, want %q", got, want)
	}
}

func TestResolveReportsUnbornAndDetachedBranches(t *testing.T) {
	root := t.TempDir()
	if err := exec.Command("git", "-c", "init.defaultBranch=initial", "init", "-q", root).Run(); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	info, err := Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	if info.Branch != "initial" {
		t.Fatalf("unborn branch = %q, want initial", info.Branch)
	}
	if err := exec.Command("git", "-C", root, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-q", "-m", "initial").Run(); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("git", "-C", root, "checkout", "--detach", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	info, err = Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	if info.Branch != "" {
		t.Fatalf("detached branch = %q, want empty", info.Branch)
	}
}

func TestResolveSharesTrustAcrossLinkedWorktrees(t *testing.T) {
	parent := t.TempDir()
	mainRoot := filepath.Join(parent, "main")
	linkedRoot := filepath.Join(parent, "linked")
	if err := exec.Command("git", "init", "-q", mainRoot).Run(); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	for key, value := range map[string]string{"user.email": "test@example.com", "user.name": "Test"} {
		if err := exec.Command("git", "-C", mainRoot, "config", key, value).Run(); err != nil {
			t.Fatal(err)
		}
	}
	if err := exec.Command("git", "-C", mainRoot, "commit", "--allow-empty", "-q", "-m", "initial").Run(); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "-C", mainRoot, "worktree", "add", "-q", "-b", "linked", linkedRoot).CombinedOutput(); err != nil {
		t.Fatalf("add worktree: %v: %s", err, output)
	}

	mainInfo, err := Resolve(mainRoot)
	if err != nil {
		t.Fatal(err)
	}
	linkedInfo, err := Resolve(linkedRoot)
	if err != nil {
		t.Fatal(err)
	}
	if mainInfo.Root == linkedInfo.Root {
		t.Fatalf("worktree roots unexpectedly equal: %q", mainInfo.Root)
	}
	if mainInfo.TrustKey != linkedInfo.TrustKey {
		t.Fatalf("trust keys differ: main=%q linked=%q", mainInfo.TrustKey, linkedInfo.TrustKey)
	}
	if mainInfo.Branch == "" || linkedInfo.Branch != "linked" {
		t.Fatalf("branches: main=%q linked=%q", mainInfo.Branch, linkedInfo.Branch)
	}

	store := NewStore(filepath.Join(t.TempDir(), "notch-home"))
	if err := store.Trust(mainRoot); err != nil {
		t.Fatal(err)
	}
	trusted, err := store.IsTrusted(linkedRoot)
	if err != nil || !trusted {
		t.Fatalf("linked worktree trust=%v err=%v", trusted, err)
	}
}

func TestLegacyWorktreeRootTrustMigratesToSharedKey(t *testing.T) {
	root := t.TempDir()
	if err := exec.Command("git", "init", "-q", root).Run(); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	info, err := Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(filepath.Join(t.TempDir(), "notch-home"))
	if err := store.TrustRoot(info.Root); err != nil {
		t.Fatal(err)
	}
	trusted, err := store.IsTrustedWorkspace(info.Root, info.TrustKey)
	if err != nil || !trusted {
		t.Fatalf("legacy trust=%v err=%v", trusted, err)
	}
	trusted, err = store.IsTrustedRoot(info.TrustKey)
	if err != nil || !trusted {
		t.Fatalf("migrated trust=%v err=%v", trusted, err)
	}
}

func TestStorePersistsCanonicalRootWithPrivateModes(t *testing.T) {
	parent := t.TempDir()
	home := filepath.Join(parent, "notch-home")
	root := filepath.Join(parent, "workspace")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(parent, "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	store := NewStore(home)
	if err := store.Trust(alias); err != nil {
		t.Fatal(err)
	}
	trusted, err := store.IsTrusted(root)
	if err != nil {
		t.Fatal(err)
	}
	if !trusted {
		t.Fatal("canonical workspace was not trusted")
	}
	for path, want := range map[string]os.FileMode{home: 0o700, store.Path(): 0o600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %04o, want %04o", path, got, want)
		}
	}
	// Persistence must work through a fresh Store instance.
	trusted, err = NewStore(home).IsTrusted(alias)
	if err != nil || !trusted {
		t.Fatalf("persisted trust = %v, %v", trusted, err)
	}
}

func TestStoreUsesGitRootForNestedPaths(t *testing.T) {
	root := t.TempDir()
	if err := exec.Command("git", "init", "-q", root).Run(); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	store := NewStore(filepath.Join(t.TempDir(), "notch-home"))
	if err := store.Trust(nested); err != nil {
		t.Fatal(err)
	}
	trusted, err := store.IsTrusted(root)
	if err != nil || !trusted {
		t.Fatalf("Git root trust = %v, %v", trusted, err)
	}
}

func TestStoreRejectsTrustDatabaseWithUnsafeMode(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	path := NewStore(home).Path()
	if err := os.WriteFile(path, []byte(`{"version":1,"workspaces":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(home).IsTrusted(root); err == nil {
		t.Fatal("unsafe trust database mode was accepted")
	}
}

func TestTrustRootRepairsUnsafeTrustDatabaseMode(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	store := NewStore(home)
	if err := os.Chmod(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.Path(), []byte(`{"version":1,"workspaces":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.TrustRoot(root); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("trust file mode = %04o, want 0600", info.Mode().Perm())
	}
	trusted, err := store.IsTrustedRoot(root)
	if err != nil || !trusted {
		t.Fatalf("trusted=%v err=%v", trusted, err)
	}
}

func TestExactRootMethodsDoNotRediscoverGitRoot(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	store := NewStore(filepath.Join(t.TempDir(), "notch-home"))
	if err := store.TrustRoot(nested); err != nil {
		t.Fatal(err)
	}
	trusted, err := store.IsTrustedRoot(nested)
	if err != nil || !trusted {
		t.Fatalf("exact root trust=%v err=%v", trusted, err)
	}
}

func TestRootFallbackRejectsUnsafeGitMarkers(t *testing.T) {
	for name, create := range map[string]func(string) error{
		"symlink": func(marker string) error { return os.Symlink(t.TempDir(), marker) },
		"malformed-file": func(marker string) error {
			return os.WriteFile(marker, []byte("not a worktree marker\n"), 0o600)
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			nested := filepath.Join(root, "nested")
			if err := os.Mkdir(nested, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := create(filepath.Join(root, ".git")); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", "") // force Root's marker fallback
			if _, err := Root(nested); err == nil {
				t.Fatalf("%s .git marker was accepted", name)
			}
		})
	}
}

func TestRootFallbackAcceptsDirectoryAndWorktreeFile(t *testing.T) {
	for name, create := range map[string]func(string) error{
		"directory": func(marker string) error { return os.Mkdir(marker, 0o755) },
		"worktree-file": func(marker string) error {
			gitDir := filepath.Join(filepath.Dir(marker), "git-data", "worktrees", "test")
			if err := os.MkdirAll(gitDir, 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(gitDir, "commondir"), []byte("../..\n"), 0o600); err != nil {
				return err
			}
			return os.WriteFile(marker, []byte("gitdir: "+gitDir+"\n"), 0o600)
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			nested := filepath.Join(root, "nested")
			if err := os.Mkdir(nested, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := create(filepath.Join(root, ".git")); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", "")
			got, err := Root(nested)
			if err != nil {
				t.Fatal(err)
			}
			if got != root {
				t.Fatalf("Root = %q, want %q", got, root)
			}
		})
	}
}

func TestHasProjectInputs(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{filepath.Join(root, ".notch", "extensions"), filepath.Join(root, ".agents", "skills")} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	present, err := HasProjectInputs(root)
	if err != nil || present {
		t.Fatalf("empty directories: present=%v err=%v", present, err)
	}
	if err := os.WriteFile(filepath.Join(root, ".agents", "skills", "test.md"), []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	present, err = HasProjectInputs(root)
	if err != nil || !present {
		t.Fatalf("resource: present=%v err=%v", present, err)
	}
}

func TestHasProjectInputsIgnoresUnrelatedAgentsFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".agents", "unrelated"), []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	present, err := HasProjectInputs(root)
	if err != nil || present {
		t.Fatalf("present=%v err=%v", present, err)
	}
}

func TestHasProjectInputsCountsConfigAndMCP(t *testing.T) {
	for _, relative := range []string{filepath.Join(".notch", "config.json"), filepath.Join(".notch", "mcp.json")} {
		t.Run(relative, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, relative)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
			present, err := HasProjectInputs(root)
			if err != nil || !present {
				t.Fatalf("present=%v err=%v", present, err)
			}
		})
	}
}

func TestHasProjectInputsCountsAgentInstructions(t *testing.T) {
	for _, name := range []string{"AGENTS.md", "AGENTS.local.md"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, name), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			present, err := HasProjectInputs(root)
			if err != nil || !present {
				t.Fatalf("present=%v err=%v", present, err)
			}
		})
	}
}

func TestInstructionsLoadsSharedThenLocal(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("shared guidance\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "AGENTS.local.md"), []byte("local override\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	instructions, err := Instructions(root)
	if err != nil {
		t.Fatal(err)
	}
	want := "## Workspace instructions from AGENTS.md\n\nshared guidance\n\n## Workspace instructions from AGENTS.local.md\n\nlocal override"
	if instructions != want {
		t.Fatalf("instructions = %q, want %q", instructions, want)
	}
}

func TestInstructionsHandlesMissingAndOversizedFiles(t *testing.T) {
	root := t.TempDir()
	instructions, err := Instructions(root)
	if err != nil || instructions != "" {
		t.Fatalf("missing instructions = %q, %v", instructions, err)
	}
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(strings.Repeat("x", maxInstructionsSize+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Instructions(root); err == nil || !strings.Contains(err.Error(), "exceed") {
		t.Fatalf("oversized instructions error = %v", err)
	}
}

func TestInstructionsEnforcesAggregateSize(t *testing.T) {
	root := t.TempDir()
	sharedSize := maxInstructionsSize / 2
	localSize := maxInstructionsSize - sharedSize
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(strings.Repeat("x", sharedSize)), 0o600); err != nil {
		t.Fatal(err)
	}
	localPath := filepath.Join(root, "AGENTS.local.md")
	if err := os.WriteFile(localPath, []byte(strings.Repeat("y", localSize)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Instructions(root); err != nil {
		t.Fatalf("exact size limit: %v", err)
	}
	if err := os.WriteFile(localPath, []byte(strings.Repeat("y", localSize+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Instructions(root); err == nil || !strings.Contains(err.Error(), "exceed") {
		t.Fatalf("aggregate size error = %v", err)
	}
}
