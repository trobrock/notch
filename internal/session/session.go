// Package session provides an append-only JSON Lines store for conversations.
package session

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/trobrock/notch/internal/model"
)

const (
	formatVersion = 1
	fileExtension = ".jsonl"
)

// Header is the first record in every session file.
type Header struct {
	Type      string    `json:"type"`
	Version   int       `json:"version"`
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	CWD       string    `json:"cwd"`
	Provider  string    `json:"provider"`
	Model     string    `json:"model"`
}

// Metadata is retained as a descriptive alias for Header.
type Metadata = Header

// Entry is the on-disk representation used by AppendMessage. Other kinds of
// entries may be appended with AppendEntry and are retained in Session.Entries.
type Entry struct {
	Type      string         `json:"type"`
	Timestamp time.Time      `json:"timestamp,omitempty"`
	Message   *model.Message `json:"message,omitempty"`
}

// CustomEntry is the envelope produced by AppendEntry when called with a kind
// and a value, for example AppendEntry("summary", summary).
type CustomEntry struct {
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	Data      any       `json:"data"`
}

// Session is a loaded or newly-created session. Entries contains the exact
// JSON for every record after the header, and Messages contains message
// records in file order.
type Session struct {
	Header   Header
	Metadata Metadata
	Entries  []json.RawMessage
	Messages []model.Message

	mu     sync.Mutex
	path   string
	file   *os.File
	closed bool
}

// New atomically creates a session in dir. The header is durable before New
// returns successfully.
func New(dir, cwd, provider, modelName string) (*Session, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create session directory %q: %w", dir, err)
	}

	for attempt := 0; attempt < 100; attempt++ {
		now := time.Now().UTC()
		suffix, err := randomSuffix()
		if err != nil {
			return nil, fmt.Errorf("generate session name: %w", err)
		}
		id := now.Format("20060102T150405.000000000Z") + "-" + suffix
		path := filepath.Join(dir, id+fileExtension)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("create session %q: %w", path, err)
		}

		header := Header{
			Type: "metadata", Version: formatVersion, ID: id, CreatedAt: now,
			CWD: cwd, Provider: provider, Model: modelName,
		}
		if err := writeJSONLine(f, header); err != nil {
			_ = f.Close()
			_ = os.Remove(path)
			return nil, fmt.Errorf("write session header %q: %w", path, err)
		}
		if err := f.Sync(); err != nil {
			_ = f.Close()
			_ = os.Remove(path)
			return nil, fmt.Errorf("sync session header %q: %w", path, err)
		}

		return &Session{Header: header, Metadata: header, path: path, file: f}, nil
	}
	return nil, errors.New("create session: too many filename collisions")
}

// Load validates and opens an existing session for further appends.
func Load(path string) (*Session, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0)
	if err != nil {
		return nil, fmt.Errorf("open session %q: %w", path, err)
	}

	s := &Session{path: path, file: f}
	if err := s.decode(f); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("load session %q: %w", path, err)
	}
	return s, nil
}

// Latest loads the most recently modified session JSONL file in dir.
func Latest(dir string) (*Session, error) {
	items, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read session directory %q: %w", dir, err)
	}
	type candidate struct {
		path string
		mod  time.Time
	}
	var files []candidate
	for _, item := range items {
		if item.IsDir() || !strings.HasSuffix(item.Name(), fileExtension) {
			continue
		}
		info, err := item.Info()
		if err != nil {
			return nil, fmt.Errorf("stat session %q: %w", filepath.Join(dir, item.Name()), err)
		}
		files = append(files, candidate{filepath.Join(dir, item.Name()), info.ModTime()})
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("latest session in %q: %w", dir, os.ErrNotExist)
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].mod.Equal(files[j].mod) {
			return files[i].path > files[j].path
		}
		return files[i].mod.After(files[j].mod)
	})
	return Load(files[0].path)
}

// AppendMessage appends a model message and makes it durable.
func (s *Session) AppendMessage(message model.Message) error {
	now := time.Now().UTC()
	entry := Entry{Type: "message", Timestamp: now, Message: &message}
	line, err := marshalLine(entry)
	if err != nil {
		return fmt.Errorf("encode message: %w", err)
	}
	return s.appendLine(line, func() {
		s.Messages = append(s.Messages, message)
		s.Entries = append(s.Entries, cloneRaw(line[:len(line)-1]))
	})
}

// AppendEntry appends an arbitrary JSON value and makes it durable. It may
// also be called as AppendEntry(kind, value), which writes a CustomEntry
// envelope. A nil value is rejected, as are metadata records (the header must
// remain unique).
func (s *Session) AppendEntry(entry any, value ...any) error {
	if len(value) > 1 {
		return errors.New("append session entry: expected at most one value")
	}
	if len(value) == 1 {
		kind, ok := entry.(string)
		if !ok || kind == "" {
			return errors.New("append session entry: custom entry type must be a non-empty string")
		}
		entry = CustomEntry{Type: kind, Timestamp: time.Now().UTC(), Data: value[0]}
	}
	line, err := marshalLine(entry)
	if err != nil {
		return fmt.Errorf("encode session entry: %w", err)
	}
	if bytes.Equal(bytes.TrimSpace(line), []byte("null")) {
		return errors.New("encode session entry: null entry")
	}
	var kind struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &kind); err == nil && kind.Type == "metadata" {
		return errors.New("append session entry: metadata is only valid as the header")
	}
	return s.appendLine(line, func() {
		s.Entries = append(s.Entries, cloneRaw(line[:len(line)-1]))
	})
}

// Path returns the session file path.
func (s *Session) Path() string { return s.path }

// Close syncs and closes the session. It is safe to call more than once.
func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.file == nil {
		return nil
	}
	syncErr := s.file.Sync()
	closeErr := s.file.Close()
	if syncErr != nil {
		return fmt.Errorf("sync session %q: %w", s.path, syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close session %q: %w", s.path, closeErr)
	}
	return nil
}

func (s *Session) appendLine(line []byte, committed func()) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.file == nil {
		return errors.New("append session: session is closed")
	}
	if err := writeAll(s.file, line); err != nil {
		return fmt.Errorf("append session %q: %w", s.path, err)
	}
	if err := s.file.Sync(); err != nil {
		return fmt.Errorf("sync session %q: %w", s.path, err)
	}
	committed()
	return nil
}

func (s *Session) decode(r io.Reader) error {
	reader := bufio.NewReader(r)
	lineNumber := 0
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) != 0 {
			lineNumber++
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) == 0 {
				return fmt.Errorf("line %d: empty JSON record", lineNumber)
			}
			if lineNumber == 1 {
				if decodeErr := decodeStrict(trimmed, &s.Header); decodeErr != nil {
					return fmt.Errorf("line 1: invalid metadata header: %w", decodeErr)
				}
				if s.Header.Type != "metadata" && s.Header.Type != "session" {
					return fmt.Errorf("line 1: expected metadata header, got type %q", s.Header.Type)
				}
				if s.Header.Version != formatVersion {
					return fmt.Errorf("line 1: unsupported session version %d", s.Header.Version)
				}
				s.Metadata = s.Header
			} else {
				if decodeErr := validateOneJSON(trimmed); decodeErr != nil {
					return fmt.Errorf("line %d: invalid entry: %w", lineNumber, decodeErr)
				}
				var discriminator struct {
					Type string `json:"type"`
				}
				_ = json.Unmarshal(trimmed, &discriminator)
				if discriminator.Type == "metadata" || discriminator.Type == "session" {
					return fmt.Errorf("line %d: metadata header may only appear on line 1", lineNumber)
				}
				if discriminator.Type == "message" {
					var entry Entry
					if decodeErr := decodeStrict(trimmed, &entry); decodeErr != nil {
						return fmt.Errorf("line %d: invalid message entry: %w", lineNumber, decodeErr)
					}
					if entry.Message == nil {
						return fmt.Errorf("line %d: message entry has no message", lineNumber)
					}
					s.Messages = append(s.Messages, *entry.Message)
				}
				s.Entries = append(s.Entries, cloneRaw(trimmed))
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read line %d: %w", lineNumber+1, err)
		}
	}
	if lineNumber == 0 {
		return errors.New("missing metadata header: file is empty")
	}
	return nil
}

func randomSuffix() (string, error) {
	var value [8]byte
	if _, err := io.ReadFull(rand.Reader, value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func marshalLine(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	encoded = append(encoded, '\n')
	return encoded, nil
}

func writeJSONLine(w io.Writer, value any) error {
	line, err := marshalLine(value)
	if err != nil {
		return err
	}
	return writeAll(w, line)
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) != 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func decodeStrict(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func validateOneJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	if value == nil {
		return errors.New("null entry")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func cloneRaw(data []byte) json.RawMessage {
	return append(json.RawMessage(nil), data...)
}
