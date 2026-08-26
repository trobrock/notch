package mcpoauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

const storeVersion = 1

type storeDocument struct {
	Version     int                   `json:"version"`
	Credentials map[string]Credential `json:"credentials"`
}

// Store atomically persists MCP OAuth credentials in a mode-0600 JSON file.
type Store struct {
	path string
	mu   sync.Mutex
}

func NewStore(path string) *Store { return &Store{path: path} }
func (s *Store) Path() string     { return s.path }

// Get returns name's credential only when it is bound to serverURL. This check
// prevents a project config from redirecting a globally stored bearer token.
func (s *Store) Get(name, serverURL string) (Credential, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	document, err := s.read()
	if err != nil {
		return Credential{}, false, err
	}
	credential, ok := document.Credentials[name]
	if !ok {
		return Credential{}, false, nil
	}
	if credential.ServerURL != serverURL {
		return Credential{}, false, fmt.Errorf("stored MCP OAuth credential for %q is bound to %s, not %s", name, credential.ServerURL, serverURL)
	}
	return credential, true, nil
}

func (s *Store) Put(name string, credential Credential) error {
	if name == "" {
		return errors.New("MCP server name is empty")
	}
	if err := validateCredential(credential); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	document, err := s.read()
	if err != nil {
		return err
	}
	document.Credentials[name] = credential
	return s.write(document)
}

func (s *Store) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	document, err := s.read()
	if err != nil {
		return err
	}
	if _, ok := document.Credentials[name]; !ok {
		return nil
	}
	delete(document.Credentials, name)
	return s.write(document)
}

// Credentials returns a copy of all stored credentials.
func (s *Store) Credentials() (map[string]Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	document, err := s.read()
	if err != nil {
		return nil, err
	}
	credentials := make(map[string]Credential, len(document.Credentials))
	for name, credential := range document.Credentials {
		credentials[name] = credential
	}
	return credentials, nil
}

func (s *Store) GetAny(name string) (Credential, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	document, err := s.read()
	if err != nil {
		return Credential{}, false, err
	}
	credential, ok := document.Credentials[name]
	return credential, ok, nil
}

func (s *Store) read() (storeDocument, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return storeDocument{Version: storeVersion, Credentials: make(map[string]Credential)}, nil
	}
	if err != nil {
		return storeDocument{}, fmt.Errorf("read MCP OAuth credentials %q: %w", s.path, err)
	}
	info, err := os.Lstat(s.path)
	if err != nil {
		return storeDocument{}, fmt.Errorf("inspect MCP OAuth credentials %q: %w", s.path, err)
	}
	if !info.Mode().IsRegular() {
		return storeDocument{}, fmt.Errorf("MCP OAuth credentials %q must be a regular file", s.path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return storeDocument{}, fmt.Errorf("MCP OAuth credentials %q must not be accessible by group or others", s.path)
	}
	var document storeDocument
	if err := json.Unmarshal(data, &document); err != nil {
		return storeDocument{}, fmt.Errorf("parse MCP OAuth credentials %q: %w", s.path, err)
	}
	if document.Version != storeVersion {
		return storeDocument{}, fmt.Errorf("unsupported MCP OAuth credential store version %d", document.Version)
	}
	if document.Credentials == nil {
		document.Credentials = make(map[string]Credential)
	}
	return document, nil
}

func (s *Store) write(document storeDocument) error {
	directory := filepath.Dir(s.path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create MCP OAuth credential directory %q: %w", directory, err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return fmt.Errorf("secure MCP OAuth credential directory %q: %w", directory, err)
	}
	document.Version = storeVersion
	if document.Credentials == nil {
		document.Credentials = make(map[string]Credential)
	}
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return fmt.Errorf("encode MCP OAuth credentials: %w", err)
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(directory, ".mcp-auth-*")
	if err != nil {
		return fmt.Errorf("create temporary MCP OAuth credential file: %w", err)
	}
	temporaryPath := temporary.Name()
	keep := false
	defer func() {
		_ = temporary.Close()
		if !keep {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, s.path); err != nil {
		return fmt.Errorf("replace MCP OAuth credentials %q: %w", s.path, err)
	}
	keep = true
	if err := os.Chmod(s.path, 0o600); err != nil {
		return fmt.Errorf("secure MCP OAuth credentials %q: %w", s.path, err)
	}
	if handle, err := os.Open(directory); err == nil {
		_ = handle.Sync()
		_ = handle.Close()
	}
	return nil
}
