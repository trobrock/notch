package pricing

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/trobrock/notch/internal/model"
)

const (
	modelsURL        = "https://models.dev/api.json"
	maxSnapshotBytes = 16 << 20
)

// Service uses a locally cached models.dev snapshot, falling back to the bundled
// table. Construction and estimation never use the network. Refresh is explicit;
// a disabled service uses only the bundled table and never reads or writes cache.
// The cache is the unmodified API JSON; its modification time is its refresh time.
type Service struct {
	path      string
	ttl       time.Duration
	disabled  bool
	client    *http.Client
	url       string
	refreshMu sync.Mutex
	mu        sync.RWMutex
	snapshot  *snapshot
	updated   time.Time
}

type snapshot struct {
	providers map[string]map[string]snapshotEntry
	version   string
}

type snapshotRates struct {
	Input      *float64 `json:"input"`
	Output     *float64 `json:"output"`
	CacheRead  *float64 `json:"cache_read"`
	CacheWrite *float64 `json:"cache_write"`
}

type snapshotTier struct {
	above int
	rates snapshotRates
}

type snapshotEntry struct {
	rates snapshotRates
	tiers []snapshotTier
}

func NewService(path string, ttl time.Duration, disabled bool) *Service {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	s := &Service{path: path, ttl: ttl, disabled: disabled, url: modelsURL, client: &http.Client{Timeout: 15 * time.Second}}
	if disabled || path == "" {
		return s
	}
	f, err := os.Open(path)
	if err != nil {
		return s
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() > maxSnapshotBytes {
		return s
	}
	data, err := readSnapshot(f)
	if err != nil {
		return s
	}
	if cached, err := parseSnapshot(data); err == nil {
		s.snapshot, s.updated = cached, info.ModTime()
	}
	return s
}

// Refresh fetches at most once per TTL unless forced. Errors leave the previous
// snapshot usable, including after its TTL has expired. Concurrent refreshes are
// serialized, but estimates remain available while HTTP is in flight.
func (s *Service) Refresh(ctx context.Context, force bool) error {
	if s.disabled {
		return nil
	}
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	s.mu.RLock()
	fresh := s.snapshot != nil && time.Since(s.updated) < s.ttl
	s.mu.RUnlock()
	if !force && fresh {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return fmt.Errorf("pricing refresh: %w", err)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("pricing refresh: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("pricing refresh: HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > maxSnapshotBytes {
		return fmt.Errorf("pricing refresh: snapshot exceeds %d bytes", maxSnapshotBytes)
	}
	data, err := readSnapshot(resp.Body)
	if err != nil {
		return fmt.Errorf("pricing refresh: %w", err)
	}
	next, err := parseSnapshot(data)
	if err != nil {
		return fmt.Errorf("pricing refresh: %w", err)
	}
	if s.path != "" {
		if err := writeSnapshot(s.path, data); err != nil {
			return fmt.Errorf("pricing cache: %w", err)
		}
	}
	s.mu.Lock()
	s.snapshot, s.updated = next, time.Now()
	s.mu.Unlock()
	return nil
}

// Estimate returns USD, the exact snapshot attribution, and whether all used
// rates are known. Missing cache prices are not free prices. Bundled estimates
// keep their own version even when a remote snapshot has been loaded.
func (s *Service) Estimate(provider, modelID, retention string, usage model.Response) (float64, string, bool) {
	if usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.CacheReadTokens < 0 || usage.CacheWriteTokens < 0 {
		return 0, "", false
	}
	family := providerFamily(provider)
	if family == "" {
		return 0, "", false
	}
	s.mu.RLock()
	current := s.snapshot
	s.mu.RUnlock()
	if !s.disabled && current != nil {
		if value, ok := lookupSnapshot(current.providers[family], modelID); ok {
			rates := value.rates
			for _, t := range value.tiers {
				if usage.TotalInputTokens() > t.above {
					rates = t.rates
				}
			}
			if family == "anthropic" && retention == "long" {
				rates.CacheWrite = nil
				if rates.Input != nil {
					write := *rates.Input * 2
					rates.CacheWrite = &write
				}
			}
			if cost, ok := snapshotCost(rates, usage); ok {
				return cost, current.version, true
			}
		}
	}
	if cost, ok := Estimate(provider, modelID, retention, usage); ok {
		return cost, Version, true
	}
	return 0, "", false
}

func snapshotCost(r snapshotRates, usage model.Response) (float64, bool) {
	cost := 0.0
	for _, v := range []struct {
		tokens int
		rate   *float64
	}{
		{usage.InputTokens, r.Input}, {usage.OutputTokens, r.Output},
		{usage.CacheReadTokens, r.CacheRead}, {usage.CacheWriteTokens, r.CacheWrite},
	} {
		if v.tokens != 0 {
			if v.rate == nil {
				return 0, false
			}
			cost += float64(v.tokens) * *v.rate / 1_000_000
		}
	}
	return cost, !math.IsNaN(cost) && !math.IsInf(cost, 0)
}

func lookupSnapshot(values map[string]snapshotEntry, modelID string) (snapshotEntry, bool) {
	id := strings.ToLower(strings.TrimSpace(modelID))
	if value, ok := values[id]; ok {
		return value, true
	}
	// Choose the longest matching base deterministically.
	base := ""
	for key := range values {
		if len(key) > len(base) && strings.HasPrefix(id, key+"-20") {
			base = key
		}
	}
	value, ok := values[base]
	return value, ok
}

func validRates(r snapshotRates) bool {
	if r.Input == nil || r.Output == nil {
		return false
	}
	for _, v := range []*float64{r.Input, r.Output, r.CacheRead, r.CacheWrite} {
		if v != nil && (*v < 0 || math.IsNaN(*v) || math.IsInf(*v, 0)) {
			return false
		}
	}
	return true
}

func parseSnapshot(data []byte) (*snapshot, error) {
	var providers map[string]struct {
		Models map[string]struct {
			Cost json.RawMessage `json:"cost"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &providers); err != nil {
		return nil, err
	}
	result := &snapshot{providers: make(map[string]map[string]snapshotEntry), version: fmt.Sprintf("models.dev-sha256:%x", sha256.Sum256(data))}
	count := 0
	for provider, p := range providers {
		models := make(map[string]snapshotEntry)
		for id, m := range p.Models {
			var cost struct {
				snapshotRates
				Tiers []struct {
					snapshotRates
					Tier struct {
						Type string `json:"type"`
						Size int    `json:"size"`
					} `json:"tier"`
				} `json:"tiers"`
				Legacy *snapshotRates `json:"context_over_200k"`
			}
			if err := json.Unmarshal(m.Cost, &cost); err != nil || !validRates(cost.snapshotRates) {
				continue
			}
			value := snapshotEntry{rates: cost.snapshotRates}
			valid := true
			seen := make(map[int]bool)
			for _, t := range cost.Tiers {
				if t.Tier.Type != "context" {
					valid = false
					break
				}
				if t.Tier.Size <= 0 || seen[t.Tier.Size] || !validRates(t.snapshotRates) {
					valid = false
					break
				}
				seen[t.Tier.Size] = true
				value.tiers = append(value.tiers, snapshotTier{above: t.Tier.Size, rates: t.snapshotRates})
			}
			// Older snapshots only exposed this fixed-threshold field. Prefer the
			// modern list when present (it may use a different context threshold).
			if len(value.tiers) == 0 && cost.Legacy != nil {
				if !validRates(*cost.Legacy) {
					valid = false
				} else {
					value.tiers = append(value.tiers, snapshotTier{above: 200_000, rates: *cost.Legacy})
				}
			}
			if !valid {
				continue
			}
			sort.Slice(value.tiers, func(i, j int) bool { return value.tiers[i].above < value.tiers[j].above })
			models[strings.ToLower(strings.TrimSpace(id))] = value
			count++
		}
		result.providers[strings.ToLower(strings.TrimSpace(provider))] = models
	}
	if count == 0 {
		return nil, fmt.Errorf("snapshot has no valid prices")
	}
	return result, nil
}

func readSnapshot(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxSnapshotBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxSnapshotBytes {
		return nil, fmt.Errorf("snapshot exceeds %d bytes", maxSnapshotBytes)
	}
	return data, nil
}

func writeSnapshot(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".pricing-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
