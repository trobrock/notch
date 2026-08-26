package credentials

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStorePutGetDeleteAndMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "credentials.json")
	store := New(path)
	want := Credential{Type: "oauth", Access: "access-secret", Refresh: "refresh-secret", Expires: 1234, AccountID: "acct"}
	if err := store.Put("openai-codex", want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.Get("openai-codex")
	if err != nil || !ok || got != want {
		t.Fatalf("Get() = %+v, %v, %v; want %+v, true, nil", got, ok, err, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
	if err := store.Delete("openai-codex"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.Get("openai-codex"); err != nil || ok {
		t.Fatalf("credential remains after Delete: ok=%v err=%v", ok, err)
	}
	if err := store.Delete("missing"); err != nil {
		t.Fatalf("deleting absent credential: %v", err)
	}
}

func TestPutAtomicallyReplacesAndRepairsMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(path, []byte(`{"old":{"type":"api_key","access":"x"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	store := New(path)
	if err := store.Put("new", Credential{Type: "api_key", Access: "y"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
	if _, ok, _ := store.Get("old"); !ok {
		t.Fatal("Put discarded another provider")
	}
}

func TestGetWithLegacyFallbackCopiesAnthropicCredential(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	store := New(path)
	legacy := Credential{Type: "oauth", Access: "a", Refresh: "r", Expires: 99, AccountID: "id"}
	if err := store.Put(LegacyAnthropicProvider, legacy); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.GetWithLegacyFallback(AnthropicClaudeCodeProvider, LegacyAnthropicProvider)
	if err != nil || !ok || got != legacy {
		t.Fatalf("GetWithLegacyFallback() = %+v, %v, %v; want %+v, true, nil", got, ok, err, legacy)
	}
	copied, ok, err := store.Get(AnthropicClaudeCodeProvider)
	if err != nil || !ok || copied != legacy {
		t.Fatalf("copied credential = %+v, %v, %v", copied, ok, err)
	}
	preserved, ok, err := store.Get(LegacyAnthropicProvider)
	if err != nil || !ok || preserved != legacy {
		t.Fatalf("legacy credential = %+v, %v, %v", preserved, ok, err)
	}
}

func TestImportPiAcceptsCamelCaseAccountID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	piPath := filepath.Join(t.TempDir(), "pi-auth.json")
	if err := os.WriteFile(piPath, []byte(`{"openai-codex":{"type":"oauth","access":"token","refresh":"refresh","expires":123,"accountId":"acct_123"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := New(path)
	if err := store.ImportPi(piPath); err != nil {
		t.Fatal(err)
	}
	credential, ok, err := store.Get("openai-codex")
	if err != nil || !ok {
		t.Fatalf("Get = %#v, %v, %v", credential, ok, err)
	}
	if credential.AccountID != "acct_123" {
		t.Fatalf("account ID = %q", credential.AccountID)
	}
}

func TestImportPiMergesWithoutSecretInErrors(t *testing.T) {
	dir := t.TempDir()
	store := New(filepath.Join(dir, "credentials.json"))
	if err := store.Put("existing", Credential{Type: "api_key", Access: "old"}); err != nil {
		t.Fatal(err)
	}
	pi := filepath.Join(dir, "pi-auth.json")
	if err := os.WriteFile(pi, []byte(`{"anthropic":{"type":"oauth","access":"a","refresh":"r","expires":99,"account_id":"id"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.ImportPi(pi); err != nil {
		t.Fatal(err)
	}
	if got, ok, _ := store.Get("anthropic"); !ok || got.Access != "a" || got.AccountID != "id" {
		t.Fatalf("imported credential = %+v, ok=%v", got, ok)
	}
	if got, ok, _ := store.Get(AnthropicClaudeCodeProvider); !ok || got.Access != "a" || got.AccountID != "id" {
		t.Fatalf("migrated credential = %+v, ok=%v", got, ok)
	}
	if _, ok, _ := store.Get("existing"); !ok {
		t.Fatal("import discarded existing provider")
	}

	secret := "do-not-print-this-secret"
	if err := os.WriteFile(pi, []byte(`{"anthropic":{"access":"`+secret+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := store.ImportPi(pi)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("malformed import error leaked input: %v", err)
	}
}
