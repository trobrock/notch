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
	"reflect"
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

// Info is lightweight session metadata used by resume pickers.
type Info struct {
	Path         string
	Header       Header
	ModifiedAt   time.Time
	MessageCount int
	Preview      string
}

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

// CompactionEntry replaces the conversation context with a compacted summary
// and the messages needed to continue the conversation.
type CompactionEntry struct {
	Type      string          `json:"type"`
	Timestamp time.Time       `json:"timestamp"`
	Summary   string          `json:"summary"`
	Messages  []model.Message `json:"messages"`
	Auto      bool            `json:"auto"`
}

// ResetEntry clears the conversation context.
type ResetEntry struct {
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
}

// TokenUsage is the provider-reported token count for one model response.
type TokenUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type DelegatedUsage struct {
	Turns        int   `json:"turns"`
	InputTokens  int   `json:"input_tokens"`
	OutputTokens int   `json:"output_tokens"`
	WallMS       int64 `json:"wall_ms"`
	Calls        int   `json:"calls"`
}

// UsageEntry records provider usage for one completed model turn.
type UsageEntry struct {
	Type       string          `json:"type"`
	Timestamp  time.Time       `json:"timestamp"`
	Provider   string          `json:"provider,omitempty"`
	Model      string          `json:"model"`
	Usage      TokenUsage      `json:"usage"`
	Delegated  *DelegatedUsage `json:"delegated,omitempty"`
	StopReason string          `json:"stop_reason,omitempty"`
}

type sessionFile interface {
	io.Writer
	Stat() (os.FileInfo, error)
	Sync() error
	Truncate(size int64) error
	Close() error
}

// Session is a loaded or newly-created session. Entries contains the exact
// JSON for every record after the header, and Messages contains the effective
// conversation context after applying message, compaction, and reset records.
type Session struct {
	Header       Header
	Metadata     Metadata
	Entries      []json.RawMessage
	Messages     []model.Message
	UsageEntries []UsageEntry

	mu     sync.Mutex
	path   string
	file   sessionFile
	closed bool
}

// New atomically creates a session in dir. The header is durable before New
// returns successfully.
func New(dir, cwd, provider, modelName string) (*Session, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create session directory %q: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("secure session directory %q: %w", dir, err)
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

// Load validates and opens an existing session for further appends. A malformed
// final record is treated as a torn write only when it is not newline
// terminated and every preceding record is valid.
func Load(path string) (*Session, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0)
	if err != nil {
		return nil, fmt.Errorf("open session %q: %w", path, err)
	}

	stat, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("stat session %q: %w", path, err)
	}
	loaded, recovery, err := readRecoverableSession(f, path, stat.Size())
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	switch recovery.kind {
	case tailRecoveryTruncate:
		if err := f.Truncate(recovery.size); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("truncate torn session tail %q: %w", path, err)
		}
		if err := f.Sync(); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("sync recovered session %q: %w", path, err)
		}
	case tailRecoveryNewline:
		if err := writeAll(f, []byte{'\n'}); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("normalize session tail %q: %w", path, err)
		}
		if err := f.Sync(); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("sync normalized session %q: %w", path, err)
		}
	}
	loaded.file = f
	return loaded, nil
}

type tailRecoveryKind uint8

const (
	tailRecoveryNone tailRecoveryKind = iota
	tailRecoveryTruncate
	tailRecoveryNewline
)

type tailRecovery struct {
	kind tailRecoveryKind
	size int64
}

// readRecoverableSession applies Load's validation rules without modifying the
// file. Callers may then perform the returned recovery action when appropriate.
func readRecoverableSession(f *os.File, path string, size int64) (*Session, tailRecovery, error) {
	unterminated := false
	if size > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], size-1); err != nil {
			return nil, tailRecovery{}, fmt.Errorf("inspect session tail %q: %w", path, err)
		}
		unterminated = last[0] != '\n'
	}

	if unterminated {
		tailStart, err := finalRecordStart(f, size)
		if err != nil {
			return nil, tailRecovery{}, fmt.Errorf("inspect session tail %q: %w", path, err)
		}
		tail := make([]byte, size-tailStart)
		if _, err := f.ReadAt(tail, tailStart); err != nil {
			return nil, tailRecovery{}, fmt.Errorf("read session tail %q: %w", path, err)
		}
		if !json.Valid(bytes.TrimSpace(tail)) {
			// Validate the prefix before discarding anything. This ensures an
			// interior corrupt record can never be mistaken for a torn tail.
			loaded := &Session{path: path}
			if err := loaded.decode(io.NewSectionReader(f, 0, tailStart)); err != nil {
				return nil, tailRecovery{}, fmt.Errorf("load session %q: %w", path, err)
			}
			return loaded, tailRecovery{kind: tailRecoveryTruncate, size: tailStart}, nil
		}
	}

	loaded := &Session{path: path}
	if err := loaded.decode(io.NewSectionReader(f, 0, size)); err != nil {
		return nil, tailRecovery{}, fmt.Errorf("load session %q: %w", path, err)
	}
	if unterminated {
		return loaded, tailRecovery{kind: tailRecoveryNewline}, nil
	}
	return loaded, tailRecovery{kind: tailRecoveryNone}, nil
}

// finalRecordStart returns the byte immediately after the last newline before
// size, or zero when the file consists of one unterminated record.
func finalRecordStart(f *os.File, size int64) (int64, error) {
	const blockSize int64 = 32 * 1024
	for end := size; end > 0; {
		start := end - blockSize
		if start < 0 {
			start = 0
		}
		block := make([]byte, end-start)
		if _, err := f.ReadAt(block, start); err != nil {
			return 0, err
		}
		if index := bytes.LastIndexByte(block, '\n'); index >= 0 {
			return start + int64(index) + 1, nil
		}
		end = start
	}
	return 0, nil
}

// List returns valid sessions ordered from most recently modified to oldest.
func List(dir string) ([]Info, error) {
	items, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read session directory %q: %w", dir, err)
	}
	infos := make([]Info, 0, len(items))
	for _, item := range items {
		if item.IsDir() || !strings.HasSuffix(item.Name(), fileExtension) {
			continue
		}
		path := filepath.Join(dir, item.Name())
		file, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("inspect session %q: %w", path, err)
		}
		stat, statErr := file.Stat()
		if statErr != nil {
			_ = file.Close()
			return nil, fmt.Errorf("stat session %q: %w", path, statErr)
		}
		loaded, _, decodeErr := readRecoverableSession(file, path, stat.Size())
		closeErr := file.Close()
		if closeErr != nil {
			return nil, fmt.Errorf("close session %q: %w", path, closeErr)
		}
		if decodeErr != nil {
			// One damaged session must not make every other session unusable.
			// Recoverable final tails are accepted by readRecoverableSession but
			// are left untouched; Load performs the actual repair.
			continue
		}
		info := Info{Path: path, Header: loaded.Header, ModifiedAt: stat.ModTime(), MessageCount: len(loaded.Messages)}
		info.Preview = sessionPreview(loaded.Messages)
		infos = append(infos, info)
	}
	sort.Slice(infos, func(i, j int) bool {
		if infos[i].ModifiedAt.Equal(infos[j].ModifiedAt) {
			return infos[i].Header.ID > infos[j].Header.ID
		}
		return infos[i].ModifiedAt.After(infos[j].ModifiedAt)
	})
	return infos, nil
}

// Resolve finds a session by exact path, ID, filename, or an unambiguous ID prefix.
func Resolve(dir, query string) (string, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return "", errors.New("resume session: empty session")
	}
	if strings.ContainsRune(query, filepath.Separator) || filepath.IsAbs(query) {
		path := query
		if !filepath.IsAbs(path) {
			path = filepath.Clean(path)
		}
		if _, err := os.Stat(path); err != nil {
			return "", fmt.Errorf("resume session %q: %w", query, err)
		}
		return path, nil
	}
	infos, err := List(dir)
	if err != nil {
		return "", err
	}
	var matches []string
	for _, info := range infos {
		filename := filepath.Base(info.Path)
		base := strings.TrimSuffix(filename, fileExtension)
		if query == info.Header.ID || query == filename || query == base {
			return info.Path, nil
		}
		if strings.HasPrefix(info.Header.ID, query) || strings.HasPrefix(base, query) {
			matches = append(matches, info.Path)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("resume session %q: ambiguous prefix (%d matches)", query, len(matches))
	}
	return "", fmt.Errorf("resume session %q: not found", query)
}

func sessionPreview(messages []model.Message) string {
	for _, message := range messages {
		if message.Role != "user" {
			continue
		}
		for _, block := range message.Content {
			if block.Type != "text" {
				continue
			}
			preview := strings.Join(strings.Fields(block.Text), " ")
			if preview == "" {
				continue
			}
			runes := []rune(preview)
			if len(runes) > 80 {
				preview = string(runes[:79]) + "…"
			}
			return preview
		}
	}
	return "(empty session)"
}

// Latest loads the most recently modified valid session JSONL file in dir.
func Latest(dir string) (*Session, error) {
	return latest(dir, "")
}

// LatestForCWD loads the most recently modified valid session whose original
// working directory matches cwd.
func LatestForCWD(dir, cwd string) (*Session, error) {
	return latest(dir, filepath.Clean(cwd))
}

func latest(dir, cwd string) (*Session, error) {
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
	var loadErr error
	sawValid := false
	for _, file := range files {
		loaded, err := Load(file.path)
		if err == nil {
			sawValid = true
			if cwd == "" || filepath.Clean(loaded.Header.CWD) == cwd {
				return loaded, nil
			}
			_ = loaded.Close()
		}
		loadErr = errors.Join(loadErr, err)
	}
	if cwd != "" && sawValid {
		return nil, fmt.Errorf("latest session for working directory %q in %q: %w", cwd, dir, os.ErrNotExist)
	}
	return nil, fmt.Errorf("latest valid session in %q: %w", dir, loadErr)
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

// AppendCompaction durably appends a compaction record, then replaces the
// in-memory conversation context with an independent copy of messages.
func (s *Session) AppendCompaction(summary string, messages []model.Message, auto bool) error {
	snapshot := cloneMessages(messages)
	entry := CompactionEntry{
		Type: "compaction", Timestamp: time.Now().UTC(), Summary: summary,
		Messages: snapshot, Auto: auto,
	}
	line, err := marshalLine(entry)
	if err != nil {
		return fmt.Errorf("encode compaction: %w", err)
	}
	return s.appendLine(line, func() {
		s.Messages = snapshot
		s.Entries = append(s.Entries, cloneRaw(line[:len(line)-1]))
	})
}

// AppendReset durably appends a reset record, then clears the in-memory
// conversation context.
func (s *Session) AppendReset() error {
	entry := ResetEntry{Type: "reset", Timestamp: time.Now().UTC()}
	line, err := marshalLine(entry)
	if err != nil {
		return fmt.Errorf("encode reset: %w", err)
	}
	return s.appendLine(line, func() {
		s.Messages = nil
		s.Entries = append(s.Entries, cloneRaw(line[:len(line)-1]))
	})
}

// AppendUsage durably records provider-reported token usage for one model turn.
func (s *Session) AppendUsage(provider, modelName string, usage TokenUsage, stopReason string, delegated ...DelegatedUsage) error {
	if strings.TrimSpace(modelName) == "" {
		return errors.New("append session usage: model is empty")
	}
	if usage.InputTokens < 0 || usage.OutputTokens < 0 {
		return errors.New("append session usage: token counts cannot be negative")
	}
	entry := UsageEntry{
		Type: "usage", Timestamp: time.Now().UTC(), Provider: provider, Model: modelName,
		Usage: usage, StopReason: stopReason,
	}
	if len(delegated) != 0 {
		value := delegated[0]
		if value.Turns < 0 || value.InputTokens < 0 || value.OutputTokens < 0 || value.WallMS < 0 || value.Calls < 0 {
			return errors.New("append session usage: delegated usage cannot be negative")
		}
		entry.Delegated = &value
	}
	line, err := marshalLine(entry)
	if err != nil {
		return fmt.Errorf("encode session usage: %w", err)
	}
	return s.appendLine(line, func() {
		s.UsageEntries = append(s.UsageEntries, entry)
		s.Entries = append(s.Entries, cloneRaw(line[:len(line)-1]))
	})
}

// AppendEntry appends an arbitrary JSON value and makes it durable. It may
// also be called as AppendEntry(kind, value), which writes a CustomEntry
// envelope. A nil value is rejected, as are metadata records (the header must
// remain unique). The kind/value form rejects core record kinds.
func (s *Session) AppendEntry(entry any, value ...any) error {
	if len(value) > 1 {
		return errors.New("append session entry: expected at most one value")
	}
	if len(value) == 1 {
		kind, ok := entry.(string)
		if !ok || strings.TrimSpace(kind) == "" {
			return errors.New("append session entry: custom entry type must be a non-empty string")
		}
		kind = strings.TrimSpace(kind)
		if reservedCustomEntryTypes[kind] {
			return fmt.Errorf("append session entry: type %q is reserved", kind)
		}
		if isNilJSONValue(value[0]) {
			return errors.New("append session entry: custom data must not be nil")
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

func isNilJSONValue(value any) bool {
	if value == nil {
		return true
	}
	reflectValue := reflect.ValueOf(value)
	switch reflectValue.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflectValue.IsNil()
	default:
		return false
	}
}

var reservedCustomEntryTypes = map[string]bool{
	"metadata":   true,
	"session":    true,
	"message":    true,
	"compaction": true,
	"reset":      true,
	"usage":      true,
}

// AppendCustomEntry appends extension-owned data in a CustomEntry envelope.
// Core session record types are reserved so custom data cannot make a session
// unreadable on its next load.
func (s *Session) AppendCustomEntry(kind string, data any) error {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return errors.New("append custom session entry: type is required")
	}
	if isNilJSONValue(data) {
		return errors.New("append custom session entry: data must not be nil")
	}
	if reservedCustomEntryTypes[kind] {
		return fmt.Errorf("append custom session entry: type %q is reserved", kind)
	}
	return s.AppendEntry(kind, data)
}

// CustomEntries returns independent copies of custom-entry data with kind in
// the current logical conversation. Records before the latest reset are omitted.
func (s *Session) CustomEntries(kind string) ([]json.RawMessage, error) {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return nil, errors.New("read custom session entries: type is required")
	}
	snapshot := s.EntriesSnapshot()
	start := 0
	for i, raw := range snapshot {
		var header struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &header); err != nil {
			continue
		}
		if header.Type == "reset" {
			start = i + 1
		}
	}
	var data []json.RawMessage
	for _, raw := range snapshot[start:] {
		var entry struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(raw, &entry); err != nil || entry.Type != kind || len(entry.Data) == 0 {
			continue
		}
		data = append(data, cloneRaw(entry.Data))
	}
	return data, nil
}

// EntriesSnapshot returns independent copies of every record after the session
// header. It is safe to call while other goroutines append entries.
func (s *Session) EntriesSnapshot() []json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := make([]json.RawMessage, len(s.Entries))
	for i, entry := range s.Entries {
		entries[i] = cloneRaw(entry)
	}
	return entries
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
	stat, err := s.file.Stat()
	if err != nil {
		return fmt.Errorf("stat session before append %q: %w", s.path, err)
	}
	originalSize := stat.Size()
	if err := writeAll(s.file, line); err != nil {
		return s.rollbackAppend(originalSize, fmt.Errorf("append session %q: %w", s.path, err))
	}
	if err := s.file.Sync(); err != nil {
		return s.rollbackAppend(originalSize, fmt.Errorf("sync session %q: %w", s.path, err))
	}
	committed()
	return nil
}

// rollbackAppend restores the last durable record boundary. A failed rollback
// leaves the file's tail in an unknown state, so the handle is permanently
// closed to prevent a later append from turning that tail into interior damage.
func (s *Session) rollbackAppend(size int64, appendErr error) error {
	if err := s.file.Truncate(size); err != nil {
		return s.disableAfterRollbackFailure(appendErr, fmt.Errorf("rollback truncate session %q to %d bytes: %w", s.path, size, err))
	}
	if err := s.file.Sync(); err != nil {
		return s.disableAfterRollbackFailure(appendErr, fmt.Errorf("rollback sync session %q after truncating to %d bytes: %w", s.path, size, err))
	}
	return appendErr
}

func (s *Session) disableAfterRollbackFailure(appendErr, rollbackErr error) error {
	s.closed = true
	file := s.file
	s.file = nil
	closeErr := file.Close()
	if closeErr != nil {
		closeErr = fmt.Errorf("close unusable session %q: %w", s.path, closeErr)
	}
	return errors.Join(appendErr, rollbackErr, closeErr)
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
				switch discriminator.Type {
				case "message":
					var entry Entry
					if decodeErr := decodeStrict(trimmed, &entry); decodeErr != nil {
						return fmt.Errorf("line %d: invalid message entry: %w", lineNumber, decodeErr)
					}
					if entry.Message == nil {
						return fmt.Errorf("line %d: message entry has no message", lineNumber)
					}
					s.Messages = append(s.Messages, *entry.Message)
				case "compaction":
					var entry CompactionEntry
					if decodeErr := decodeStrict(trimmed, &entry); decodeErr != nil {
						return fmt.Errorf("line %d: invalid compaction entry: %w", lineNumber, decodeErr)
					}
					s.Messages = cloneMessages(entry.Messages)
				case "reset":
					var entry ResetEntry
					if decodeErr := decodeStrict(trimmed, &entry); decodeErr != nil {
						return fmt.Errorf("line %d: invalid reset entry: %w", lineNumber, decodeErr)
					}
					s.Messages = nil
				case "usage":
					var entry UsageEntry
					if decodeErr := decodeStrict(trimmed, &entry); decodeErr != nil {
						return fmt.Errorf("line %d: invalid usage entry: %w", lineNumber, decodeErr)
					}
					if entry.Model == "" || entry.Usage.InputTokens < 0 || entry.Usage.OutputTokens < 0 {
						return fmt.Errorf("line %d: invalid usage entry values", lineNumber)
					}
					if entry.Delegated != nil {
						if entry.Delegated.Turns < 0 || entry.Delegated.InputTokens < 0 || entry.Delegated.OutputTokens < 0 || entry.Delegated.WallMS < 0 || entry.Delegated.Calls < 0 {
							return fmt.Errorf("line %d: invalid delegated usage values", lineNumber)
						}
					}
					s.UsageEntries = append(s.UsageEntries, entry)
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

func cloneMessages(messages []model.Message) []model.Message {
	if messages == nil {
		return nil
	}
	cloned := make([]model.Message, len(messages))
	for i, message := range messages {
		cloned[i].Role = message.Role
		if message.Content != nil {
			cloned[i].Content = make([]model.Block, len(message.Content))
			copy(cloned[i].Content, message.Content)
			for j := range cloned[i].Content {
				cloned[i].Content[j].Arguments = cloneRaw(message.Content[j].Arguments)
			}
		}
	}
	return cloned
}
