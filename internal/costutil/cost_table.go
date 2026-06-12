// Package costutil provides shared cost-table construction utilities used by
// the proxy quota subsystem and admin layer.
package costutil

import (
	"strings"

	"github.com/ycgame/llms-proxy/internal/catalog"
	"github.com/ycgame/llms-proxy/internal/nosql"
	"github.com/ycgame/llms-proxy/internal/usage"
)

// ToCostTable builds a CostTable keyed by model name only.
// Layer 1: catalog default prices as baseline.
// Layer 2: custom model_costs override (higher priority).
func ToCostTable(costs []nosql.ModelCost, cat *catalog.Catalog) usage.CostTable {
	table := make(usage.CostTable)

	// Layer 1: catalog default prices as baseline.
	if cat != nil {
		for _, entry := range cat.ListAll() {
			if entry.DefaultCost == nil || entry.Model == "" {
				continue
			}
			model := strings.ToLower(strings.TrimSpace(entry.Model))
			rates := usage.CostRates{
				InputPer1MTokens:      entry.DefaultCost.InputPer1MTokens,
				OutputPer1MTokens:     entry.DefaultCost.OutputPer1MTokens,
				CachedInputPer1MToken: entry.DefaultCost.CachedInputPer1MToken,
				CacheReadPer1MToken:   entry.DefaultCost.CacheReadPer1MToken,
			}
			table[model] = rates
		}
	}

	// Layer 2: custom model_costs override (higher priority).
	for _, cost := range costs {
		model := strings.ToLower(strings.TrimSpace(cost.Model))
		if model == "" {
			continue
		}
		rates := usage.CostRates{
			InputPer1MTokens:      cost.InputPer1MTokens,
			OutputPer1MTokens:     cost.OutputPer1MTokens,
			CachedInputPer1MToken: cost.CachedInputPer1MToken,
			CacheReadPer1MToken:   cost.CacheReadPer1MToken,
		}
		table[model] = rates
	}

	return table
}
