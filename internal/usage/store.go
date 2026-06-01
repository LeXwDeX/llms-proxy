package usage

import (
	"sort"
	"strings"
	"time"
)

// Event is one usage record item.
type Event struct {
	Timestamp    time.Time `json:"timestamp"`
	ClientName   string    `json:"client_name"`
	EndpointType string    `json:"endpoint_type,omitempty"`
	Model        string    `json:"model,omitempty"`
	InputTokens  int64     `json:"input_tokens"`
	OutputTokens int64     `json:"output_tokens"`
	CachedTokens int64     `json:"cached_tokens"`
	RequestID    string    `json:"request_id,omitempty"`
	Target       string    `json:"target,omitempty"`
	Path         string    `json:"path,omitempty"`
	StatusCode   int       `json:"status_code"`
}

// Filter controls list/aggregate query range.
type Filter struct {
	From       *time.Time
	To         *time.Time
	ClientName string
	Model      string
	Limit      int
}

// CostRates maps model to token prices (per 1M tokens).
type CostRates struct {
	InputPer1MTokens      float64
	OutputPer1MTokens     float64
	CachedInputPer1MToken float64
}

// CostTable holds rates by "endpoint_type:model" key, falling back to "model" for backward compat.
type CostTable map[string]CostRates

// LookupCost finds cost rates for an event, trying "endpoint_type:model" first, then "model".
// For dual_protocol events, it additionally tries the inferred original manufacturer's
// endpoint_type (e.g. claude:*, openai:*) before falling back to model-only lookup.
func (ct CostTable) LookupCost(endpointType, model string) (CostRates, bool) {
	endpointType = strings.ToLower(strings.TrimSpace(endpointType))
	model = strings.ToLower(strings.TrimSpace(model))

	// Exact match: endpoint_type:model
	if endpointType != "" {
		if rates, ok := ct[endpointType+":"+model]; ok {
			return rates, true
		}
	}

	// dual_protocol smart fallback: infer original manufacturer's endpoint_type
	// so that e.g. a Claude model served via dual_protocol uses claude:* pricing.
	if endpointType == "dual_protocol" {
		if original := InferOriginalEndpointType(model); original != "" {
			if rates, ok := ct[original+":"+model]; ok {
				return rates, true
			}
		}
	}

	// Fallback: model only
	if rates, ok := ct[model]; ok {
		return rates, true
	}
	return CostRates{}, false
}

// InferOriginalEndpointType infers the original manufacturer's endpoint_type from a model name.
// Returns empty string if the model cannot be mapped to a known manufacturer.
// This is used by dual_protocol cost lookup to find the correct pricing tier.
//
// Mapping covers all known model families across global and Chinese AI providers.
// The returned string matches the canonical endpoint_type used in catalog entries
// or custom model_costs, enabling accurate pricing lookup for dual_protocol events.
func InferOriginalEndpointType(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return ""
	}

	// ── Global providers ──

	// Anthropic: claude-sonnet-4-20250514, claude-opus-4-1, claude-3-5-haiku, etc.
	if strings.HasPrefix(m, "claude-") || strings.HasPrefix(m, "claude_") {
		return "claude"
	}

	// Google: gemini-2.0-flash, gemini-1.5-pro, gemma-3-27b-it, etc.
	if strings.HasPrefix(m, "gemini-") || strings.HasPrefix(m, "gemini_") ||
		strings.HasPrefix(m, "gemma-") || strings.HasPrefix(m, "gemma_") {
		return "gemini"
	}

	// OpenAI: gpt-4o, gpt-5.5, o1-mini, o3-mini, o4-mini, dall-e-3,
	// text-embedding-3-large, tts-1, whisper-1, chatgpt-image-latest, codex-mini, etc.
	if strings.HasPrefix(m, "gpt-") || strings.HasPrefix(m, "gpt_") ||
		strings.HasPrefix(m, "o1") || strings.HasPrefix(m, "o3") || strings.HasPrefix(m, "o4") ||
		strings.HasPrefix(m, "dall-e-") || strings.HasPrefix(m, "text-embedding-") ||
		strings.HasPrefix(m, "tts-") || m == "whisper-1" ||
		strings.HasPrefix(m, "chatgpt-") || strings.HasPrefix(m, "codex-") {
		return "openai"
	}

	// Mistral AI: mistral-large-2411, ministral-3b, codestral-2501, mistral-nemo, etc.
	if strings.HasPrefix(m, "mistral-") || strings.HasPrefix(m, "ministral-") ||
		strings.HasPrefix(m, "codestral-") {
		return "mistral"
	}

	// xAI: grok-3, grok-4, grok-code-fast-1, etc.
	if strings.HasPrefix(m, "grok-") || strings.HasPrefix(m, "grok_") {
		return "grok"
	}

	// Cohere: cohere-command-r, cohere-embed-v3-english, etc.
	if strings.HasPrefix(m, "cohere-") || strings.HasPrefix(m, "cohere_") {
		return "cohere"
	}

	// Meta: llama-3.3-70b-instruct, meta-llama-3-70b-instruct, etc.
	if strings.HasPrefix(m, "llama-") || strings.HasPrefix(m, "llama_") ||
		strings.HasPrefix(m, "meta-llama-") {
		return "meta"
	}

	// Microsoft: phi-3-mini, phi-4, phi-4-reasoning, etc.
	if strings.HasPrefix(m, "phi-") || strings.HasPrefix(m, "phi_") {
		return "phi"
	}

	// ── Chinese providers ──

	// DeepSeek: deepseek-v4-pro, deepseek-chat, etc.
	if strings.HasPrefix(m, "deepseek-") || strings.HasPrefix(m, "deepseek_") {
		return "deepseek"
	}

	// 智谱AI GLM: glm-4, glm-4-plus, glm-4v, etc.
	if strings.HasPrefix(m, "glm-") || strings.HasPrefix(m, "glm_") {
		return "glm"
	}

	// MiniMax: minimax-text-01, abab6.5s-chat, etc.
	if strings.HasPrefix(m, "minimax-") || strings.HasPrefix(m, "minimax_") ||
		strings.HasPrefix(m, "abab") {
		return "minimax"
	}

	// 阿里通义千问: qwen-turbo, qwen-plus, qwen-max, qwen3-235b, etc.
	if strings.HasPrefix(m, "qwen-") || strings.HasPrefix(m, "qwen_") ||
		strings.HasPrefix(m, "qwen2") || strings.HasPrefix(m, "qwen3") {
		return "qwen"
	}

	// 月之暗面 Kimi/Moonshot: kimi-k2, moonshot-v1-8k, etc.
	if strings.HasPrefix(m, "kimi-") || strings.HasPrefix(m, "kimi_") ||
		strings.HasPrefix(m, "moonshot-") {
		return "kimi"
	}

	// 零一万物: yi-large, yi-lightning, yi-vision, etc.
	if strings.HasPrefix(m, "yi-") || strings.HasPrefix(m, "yi_") {
		return "yi"
	}

	// 百川智能: baichuan2-turbo, baichuan4, baichuan-turbo, etc.
	if strings.HasPrefix(m, "baichuan") {
		return "baichuan"
	}

	// 阶跃星辰: step-1-8k, step-2-16k, etc.
	if strings.HasPrefix(m, "step-") || strings.HasPrefix(m, "step_") {
		return "step"
	}

	// 上海AI Lab 书生: internlm2-chat-7b, internlm3-8b-instruct, etc.
	if strings.HasPrefix(m, "internlm-") || strings.HasPrefix(m, "internlm_") ||
		strings.HasPrefix(m, "internlm2-") || strings.HasPrefix(m, "internlm3-") {
		return "internlm"
	}

	// 字节跳动豆包: doubao-pro-32k, doubao-lite-4k, etc.
	if strings.HasPrefix(m, "doubao-") || strings.HasPrefix(m, "doubao_") {
		return "doubao"
	}

	// 百度文心: ernie-4.0-8k, ernie-3.5-8k, ernie-speed-128k, etc.
	if strings.HasPrefix(m, "ernie-") || strings.HasPrefix(m, "ernie_") {
		return "ernie"
	}

	// 腾讯混元: hunyuan-pro, hunyuan-standard, hunyuan-lite, etc.
	if strings.HasPrefix(m, "hunyuan-") || strings.HasPrefix(m, "hunyuan_") {
		return "hunyuan"
	}

	// 科大讯飞星火: spark-pro, spark-lite, spark-max, etc.
	if strings.HasPrefix(m, "spark-") || strings.HasPrefix(m, "spark_") {
		return "spark"
	}

	return ""
}

// Totals is aggregated usage and estimated cost.
type Totals struct {
	Requests      int64   `json:"requests"`
	InputTokens   int64   `json:"input_tokens"`
	OutputTokens  int64   `json:"output_tokens"`
	CachedTokens  int64   `json:"cached_tokens"`
	EstimatedCost float64 `json:"estimated_cost"`
}

// Bucket is one time-bucket usage aggregation.
type Bucket struct {
	BucketStart time.Time `json:"bucket_start"`
	BucketEnd   time.Time `json:"bucket_end"`
	Totals
}

// DimensionTotals is one dimension aggregate item.
type DimensionTotals struct {
	Key string `json:"key"`
	Totals
}

// AggregateResult is the full aggregation output.
type AggregateResult struct {
	From     time.Time         `json:"from"`
	To       time.Time         `json:"to"`
	GroupBy  string            `json:"group_by"`
	Totals   Totals            `json:"totals"`
	Buckets  []Bucket          `json:"buckets"`
	ByClient []DimensionTotals `json:"by_client"`
	ByModel  []DimensionTotals `json:"by_model"`
}

// SummaryResult provides predefined windows for UI.
type SummaryResult struct {
	GeneratedAt time.Time `json:"generated_at"`
	LastHour    Totals    `json:"last_hour"`
	Today       Totals    `json:"today"`
	Yesterday   Totals    `json:"yesterday"`
	Last7Days   Totals    `json:"last_7_days"`
	Last30Days  Totals    `json:"last_30_days"`
}

// Recorder records one usage event.
type Recorder interface {
	Record(event Event) error
}

// --- Utility functions used by nosql.UsageStore and other aggregation code ---

// FilterEvents filters events according to filter criteria.
func FilterEvents(events []Event, filter Filter) []Event {
	clientKey := strings.TrimSpace(filter.ClientName)
	modelKey := strings.ToLower(strings.TrimSpace(filter.Model))

	filtered := make([]Event, 0, len(events))
	for _, evt := range events {
		t := evt.Timestamp.UTC()
		if filter.From != nil && t.Before(filter.From.UTC()) {
			continue
		}
		if filter.To != nil && t.After(filter.To.UTC()) {
			continue
		}
		if clientKey != "" && evt.ClientName != clientKey {
			continue
		}
		if modelKey != "" && strings.ToLower(evt.Model) != modelKey {
			continue
		}
		filtered = append(filtered, evt)
	}
	return filtered
}

// NormalizeGroupBy normalises group_by values.
func NormalizeGroupBy(groupBy string) string {
	groupBy = strings.ToLower(strings.TrimSpace(groupBy))
	if groupBy != "hour" {
		return "day"
	}
	return groupBy
}

// BucketStartFor computes the bucket boundary for the given time.
func BucketStartFor(t time.Time, groupBy string) time.Time {
	t = t.UTC()
	if groupBy == "hour" {
		return t.Truncate(time.Hour)
	}
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// StepDuration returns the duration for one bucket.
func StepDuration(groupBy string) time.Duration {
	if groupBy == "hour" {
		return time.Hour
	}
	return 24 * time.Hour
}

// SortedDimensions converts a dimension map to a sorted slice.
func SortedDimensions(input map[string]*DimensionTotals) []DimensionTotals {
	keys := make([]string, 0, len(input))
	for key := range input {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	result := make([]DimensionTotals, 0, len(keys))
	for _, key := range keys {
		result = append(result, *input[key])
	}
	return result
}

// AddEventTotals adds one event's contribution to aggregate totals.
func AddEventTotals(target *Totals, evt Event, costs CostTable) {
	target.Requests++
	target.InputTokens += evt.InputTokens
	target.OutputTokens += evt.OutputTokens
	target.CachedTokens += evt.CachedTokens
	target.EstimatedCost += EstimateEventCost(evt, costs)
}

// EstimateEventCost returns the estimated cost for a single event.
func EstimateEventCost(evt Event, costs CostTable) float64 {
	rate, ok := costs.LookupCost(evt.EndpointType, evt.Model)
	if !ok {
		return 0
	}
	return float64(evt.InputTokens)/1_000_000*rate.InputPer1MTokens +
		float64(evt.OutputTokens)/1_000_000*rate.OutputPer1MTokens +
		float64(evt.CachedTokens)/1_000_000*rate.CachedInputPer1MToken
}
