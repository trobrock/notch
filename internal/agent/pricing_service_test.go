package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/trobrock/notch/internal/model"
	"github.com/trobrock/notch/internal/pricing"
)

func TestResponseUsageCachedPricing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pricing.json")
	if err := os.WriteFile(path, []byte(`{"anthropic":{"models":{"claude-opus-5-5":{"cost":{"input":4,"output":20,"cache_read":0.2,"cache_write":5}}}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	a := &Agent{pricing: pricing.NewService(path, time.Hour, false)}
	response := model.Response{InputTokens: 1000000, APIPricingEligible: true}
	usage := a.responseUsage(response, "short", "anthropic", "claude-opus-5-5")
	if usage.CostUSD == nil || *usage.CostUSD != 4 || usage.PricingVersion == pricing.Version || usage.PricingVersion == "" {
		t.Fatalf("usage: %+v", usage)
	}
	providerCost := 2.0
	response.CostUSD = &providerCost
	usage = a.responseUsage(response, "short", "anthropic", "claude-opus-5-5")
	if *usage.CostUSD != 2 || *usage.EstimatedCostUSD != 4 || usage.CostSource != "provider" {
		t.Fatalf("usage: %+v", usage)
	}
}
