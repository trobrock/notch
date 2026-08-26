// Package credentials stores provider credentials on disk.
package credentials

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Credential is the on-disk credential representation shared by all providers.
// Expires is a Unix timestamp in milliseconds. It is zero for credentials that
// do not expire.
type Credential struct {
	Type      string `json:"type"`
	Access    string `json:"access"`
	Refresh   string `json:"refresh,omitempty"`
	Expires   int64  `json:"expires,omitempty"`
	AccountID string `json:"account_id,omitempty"`
}

// Store is a JSON credential store. A Store is safe for concurrent use within
// one process.
type Store struct {
	path string
	mu   sync.Mutex
}

// New constructs a credential store at path. The file is created by the first
// Put, Delete, or ImportPi operation that has data to write.
func New(path string) *Store { return &Store{path: path} }

// NewStore is an alias for New.
func NewStore(path string) *Store { return New(path) }

// Path returns the store's path.
func (s *Store) Path() string { return s.path }

// Get returns a provider credential and whether it was present.
func (s *Store) Get(provider string) (Credential, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.read()
	if err != nil {
		return Credential{}, false, err
	}
	credential, ok := all[provider]
	return credential, ok, nil
}

// Put creates or replaces provider's credential.
func (s *Store) Put(provider string, credential Credential) error {
	if provider == "" {
		return errors.New("credential provider is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.read()
	if err != nil {
		return err
	}
	all[provider] = credential
	return s.write(all)
}

// Delete removes provider's credential. Deleting an absent provider succeeds.
func (s *Store) Delete(provider string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.read()
	if err != nil {
		return err
	}
	if _, ok := all[provider]; !ok {
		return nil
	}
	delete(all, provider)
	return s.write(all)
}

// ImportPi merges credentials from a Pi auth file. Pi's file is expected to
// have the same provider-to-credential JSON shape as this store. Errors contain
// paths and structural information only; credential values are never included.
func (s *Store) ImportPi(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read Pi credentials %q: %w", path, err)
	}
	// Pi currently writes accountId in camelCase while Notch uses account_id.
	// Accept both so importing a Codex subscription preserves its account scope.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("parse Pi credentials %q: %w", path, err)
	}
	imported := make(map[string]Credential, len(raw))
	for provider, value := range raw {
		var wire struct {
			Type        string `json:"type"`
			Access      string `json:"access"`
			Refresh     string `json:"refresh"`
			Expires     int64  `json:"expires"`
			AccountID   string `json:"account_id"`
			PiAccountID string `json:"accountId"`
		}
		if err := json.Unmarshal(value, &wire); err != nil {
			return fmt.Errorf("parse Pi credentials %q provider %q: %w", path, provider, err)
		}
		if wire.AccountID == "" {
			wire.AccountID = wire.PiAccountID
		}
		imported[provider] = Credential{Type: wire.Type, Access: wire.Access, Refresh: wire.Refresh, Expires: wire.Expires, AccountID: wire.AccountID}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.read()
	if err != nil {
		return err
	}
	for provider, credential := range imported {
		if provider == "" {
			return fmt.Errorf("parse Pi credentials %q: empty provider name", path)
		}
		all[provider] = credential
	}
	return s.write(all)
}

func (s *Store) read() (map[string]Credential, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return make(map[string]Credential), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read credentials %q: %w", s.path, err)
	}
	var all map[string]Credential
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, fmt.Errorf("parse credentials %q: %w", s.path, err)
	}
	if all == nil {
		all = make(map[string]Credential)
	}
	return all, nil
}

func (s *Store) write(all map[string]Credential) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create credential directory %q: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("secure credential directory %q: %w", dir, err)
	}
	data, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return fmt.Errorf("encode credentials: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, ".credentials-*")
	if err != nil {
		return fmt.Errorf("create temporary credential file in %q: %w", dir, err)
	}
	tmpPath := tmp.Name()
	keep := false
	defer func() {
		_ = tmp.Close()
		if !keep {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("secure temporary credential file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temporary credential file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temporary credential file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary credential file: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("replace credentials %q: %w", s.path, err)
	}
	keep = true
	// Rename preserves the temporary file's mode, but chmod also repairs a file
	// replaced by unusual platform-specific rename behavior.
	if err := os.Chmod(s.path, 0o600); err != nil {
		return fmt.Errorf("secure credentials %q: %w", s.path, err)
	}
	if directory, err := os.Open(dir); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}
