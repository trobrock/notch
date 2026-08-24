// Package modelregistry maintains a small embedded model fallback and a durable,
// on-demand cache of models returned by provider APIs.
package modelregistry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/trobrock/notch/internal/model"
)

const cacheVersion = 1

// Entry is one selectable model.
type Entry struct {
	Provider      string `json:"provider"`
	ID            string `json:"id"`
	Name          string `json:"name,omitempty"`
	ContextWindow int    `json:"context_window,omitempty"`
	Reasoning     bool   `json:"reasoning,omitempty"`
	Source        string `json:"source,omitempty"`
}

// Fetcher discovers models from one configured provider.
type Fetcher func(context.Context) ([]model.ModelInfo, error)

type cachedProvider struct {
	Provider  string    `json:"provider"`
	UpdatedAt time.Time `json:"updated_at"`
	Models    []Entry   `json:"models"`
}

type cacheFile struct {
	Version   int                       `json:"version"`
	Providers map[string]cachedProvider `json:"providers"`
}

// Registry refreshes stale provider entries only when requested; it never owns
// a ticker or polling goroutine.
type Registry struct {
	path string
	ttl  time.Duration
	now  func() time.Time

	mu      sync.Mutex
	loaded  bool
	loadErr error
	cache   cacheFile
}

func New(path string, ttl time.Duration) *Registry {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &Registry{path: path, ttl: ttl, now: time.Now}
}

// Providers returns the supported providers in stable display order.
func Providers() []string { return []string{"openai-codex", "anthropic", "openrouter", "openai"} }

// Scope returns a cache key which separates custom provider endpoints without
// storing the endpoint itself in the cache key.
func Scope(provider, baseURL string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return provider
	}
	sum := sha256.Sum256([]byte(baseURL))
	return provider + ":" + hex.EncodeToString(sum[:6])
}

// List returns fresh cached models, refreshes a stale entry through fetch, or
// falls back to stale/bundled data when discovery fails. A non-nil error can be
// returned with usable fallback models.
func (r *Registry) List(ctx context.Context, provider, scope string, force bool, fetch Fetcher) ([]Entry, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if scope == "" {
		scope = provider
	}
	r.mu.Lock()
	r.loadLocked()
	loadErr := r.loadErr
	cached, found := r.cache.Providers[scope]
	fresh := found && r.now().Sub(cached.UpdatedAt) < r.ttl
	if fresh && !force {
		models := cloneEntries(cached.Models)
		r.mu.Unlock()
		return models, loadErr
	}
	r.mu.Unlock()

	if fetch != nil {
		discovered, err := fetch(ctx)
		if err == nil && len(discovered) != 0 {
			models := normalize(provider, discovered, "provider")
			r.mu.Lock()
			r.loadLocked()
			r.cache.Providers[scope] = cachedProvider{Provider: provider, UpdatedAt: r.now().UTC(), Models: models}
			saveErr := r.saveLocked()
			if saveErr == nil {
				r.loadErr = nil
			}
			r.mu.Unlock()
			return cloneEntries(models), errors.Join(loadErr, saveErr)
		}
		if err == nil {
			err = errors.New("provider returned no models")
		}
		fallback := cloneEntries(cached.Models)
		if len(fallback) == 0 {
			fallback = Builtin(provider)
		}
		return fallback, errors.Join(loadErr, err)
	}

	fallback := cloneEntries(cached.Models)
	if len(fallback) == 0 {
		fallback = Builtin(provider)
	}
	return fallback, loadErr
}

// Cached returns cache or bundled data without network access.
func (r *Registry) Cached(provider, scope string) ([]Entry, error) {
	return r.List(context.Background(), provider, scope, false, nil)
}

func (r *Registry) loadLocked() {
	if r.loaded {
		return
	}
	r.loaded = true
	r.cache = cacheFile{Version: cacheVersion, Providers: make(map[string]cachedProvider)}
	if strings.TrimSpace(r.path) == "" {
		return
	}
	data, err := os.ReadFile(r.path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		r.loadErr = fmt.Errorf("read model cache %q: %w", r.path, err)
		return
	}
	var decoded cacheFile
	if err := json.Unmarshal(data, &decoded); err != nil {
		r.loadErr = fmt.Errorf("decode model cache %q: %w", r.path, err)
		return
	}
	if decoded.Version != cacheVersion {
		r.loadErr = fmt.Errorf("decode model cache %q: unsupported version %d", r.path, decoded.Version)
		return
	}
	if decoded.Providers == nil {
		decoded.Providers = make(map[string]cachedProvider)
	}
	r.cache = decoded
}

func (r *Registry) saveLocked() error {
	if strings.TrimSpace(r.path) == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return fmt.Errorf("create model cache directory: %w", err)
	}
	data, err := json.MarshalIndent(r.cache, "", "  ")
	if err != nil {
		return fmt.Errorf("encode model cache: %w", err)
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(r.path), ".models-*.tmp")
	if err != nil {
		return fmt.Errorf("create model cache: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write model cache: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync model cache: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close model cache: %w", err)
	}
	if err := os.Rename(tmpPath, r.path); err != nil {
		return fmt.Errorf("replace model cache: %w", err)
	}
	return nil
}

func normalize(provider string, values []model.ModelInfo, source string) []Entry {
	byID := make(map[string]Entry, len(values))
	fallback := make(map[string]Entry)
	for _, entry := range Builtin(provider) {
		fallback[entry.ID] = entry
	}
	for _, value := range values {
		id := strings.TrimSpace(value.ID)
		if id == "" {
			continue
		}
		known := fallback[id]
		name := strings.TrimSpace(value.Name)
		if name == "" || name == id {
			if known.Name != "" {
				name = known.Name
			} else {
				name = id
			}
		}
		contextWindow := value.ContextWindow
		if contextWindow == 0 {
			contextWindow = known.ContextWindow
		}
		byID[id] = Entry{Provider: provider, ID: id, Name: name, ContextWindow: contextWindow, Reasoning: value.Reasoning || known.Reasoning, Source: source}
	}
	out := make([]Entry, 0, len(byID))
	for _, value := range byID {
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].ID) < strings.ToLower(out[j].ID) })
	return out
}

func cloneEntries(values []Entry) []Entry { return append([]Entry(nil), values...) }
