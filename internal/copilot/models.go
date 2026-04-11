package copilot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// ModelInfo 表示一个可用模型的信息
type ModelInfo struct {
	Name       string  `json:"name"`       // 上游模型名
	Multiplier float64 `json:"multiplier"` // premium requests 乘数
	Category   string  `json:"category"`   // 免费|低消耗|标准|高消耗
}

// categoryOrder 定义模型分类的显示优先级。
var categoryOrder = map[string]int{
	"免费":  0,
	"低消耗": 1,
	"标准":  2,
	"高消耗": 3,
}

// sortModelInfoByCategory 按 Category 优先级排序 ModelInfo 切片，同 Category 内按 Name 排序。
func sortModelInfoByCategory(models []ModelInfo) {
	sort.Slice(models, func(i, j int) bool {
		ci := categoryOrder[models[i].Category]
		cj := categoryOrder[models[j].Category]
		if ci != cj {
			return ci < cj
		}
		return models[i].Name < models[j].Name
	})
}

// SortModelDetailByCategory 按 Category 优先级排序 CopilotModelDetail 切片，同 Category 内按 ID 排序。
func SortModelDetailByCategory(models []CopilotModelDetail) {
	sort.Slice(models, func(i, j int) bool {
		ci := categoryOrder[models[i].Category]
		cj := categoryOrder[models[j].Category]
		if ci != cj {
			return ci < cj
		}
		return models[i].ID < models[j].ID
	})
}

// 模型名前缀（下游使用）
const ModelPrefix = "copilot_"

// ModelMultipliers 定义所有已知模型的 premium request 乘数。
// key 为小写模型名。
// 数据来源：https://docs.github.com/en/copilot/managing-copilot/monitoring-usage-and-entitlements/about-premium-requests
var ModelMultipliers = map[string]float64{
	// 免费模型（乘数 0）
	"gpt-4.1":     0,
	"gpt-4o":      0,
	"gpt-5-mini":  0,
	"raptor-mini": 0,

	// 低消耗模型（乘数 <1）
	"grok-code-fast-1": 0.25,
	"claude-haiku-4.5": 0.33,
	"gemini-3-flash":   0.33,
	"gpt-5.4-mini":     0.33,

	// 标准模型（乘数 1）
	"claude-sonnet-4":        1,
	"claude-sonnet-4.5":      1,
	"claude-sonnet-4.6":      1,
	"gemini-2.5-pro":         1,
	"gemini-3.1-pro":         1,
	"gemini-3.1-pro-preview": 1,
	"gpt-5.1":                1,
	"gpt-5.2":                1,
	"gpt-5.3-codex":          1,
	"gpt-5.4":                1,

	// 高消耗模型（乘数 >1）
	"claude-opus-4.5": 3,
	"claude-opus-4.6": 3,
}

// MapModelName 将下游模型名映射为上游模型名。
// 例如：copilot_gpt-4o → gpt-4o
// 大小写不敏感。
func MapModelName(downstreamModel string) (upstreamModel string, found bool) {
	lower := strings.ToLower(downstreamModel)
	prefix := strings.ToLower(ModelPrefix)
	if strings.HasPrefix(lower, prefix) {
		return downstreamModel[len(prefix):], true
	}
	return downstreamModel, false
}

// ReverseMapModelName 将上游模型名映射为下游模型名。
// 例如：gpt-4o → copilot_gpt-4o
func ReverseMapModelName(upstreamModel string) string {
	return ModelPrefix + upstreamModel
}

// GetMultiplier 获取模型的 premium request 乘数。
// 未知模型返回 1.0（保守策略）。
// 大小写不敏感。
func GetMultiplier(model string) float64 {
	// 如果有 copilot_ 前缀先去掉
	mapped, _ := MapModelName(model)
	lower := strings.ToLower(mapped)
	if m, ok := ModelMultipliers[lower]; ok {
		return m
	}
	return 1.0
}

// ListAvailableModels 返回所有可用模型列表。
// 按 Category 分组，Category 内按名字排序。
func ListAvailableModels() []ModelInfo {
	var models []ModelInfo
	for name, multiplier := range ModelMultipliers {
		models = append(models, ModelInfo{
			Name:       name,
			Multiplier: multiplier,
			Category:   classifyModel(multiplier),
		})
	}

	sortModelInfoByCategory(models)
	return models
}

// IsFreeModel 判断模型是否为免费模型（乘数为 0）。
func IsFreeModel(model string) bool {
	return GetMultiplier(model) == 0
}

// copilotModelsAPIResponse 表示 Copilot models API 的响应结构。
// Individual 返回 { "models": [...] }
// Business 返回 { "data": [...], "object": "list" }
type copilotModelsAPIResponse struct {
	Models []copilotModelEntry `json:"models"`
	Data   []copilotModelEntry `json:"data"`
	Object string              `json:"object"`
}

type copilotModelEntry struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version"`
	Vendor  string `json:"vendor"`
	Preview bool   `json:"preview"`

	// model_picker_enabled 标记用户是否可以选择该模型
	ModelPickerEnabled  bool   `json:"model_picker_enabled"`
	ModelPickerCategory string `json:"model_picker_category"` // versatile, powerful

	Capabilities struct {
		Type string `json:"type"` // chat, embeddings
	} `json:"capabilities"`

	SupportedEndpoints []string `json:"supported_endpoints"`

	Policy *struct {
		State string `json:"state"` // enabled, disabled
	} `json:"policy,omitempty"`
}

// fetchRawBody 发送 GET 请求到 Copilot models API，返回原始响应 body。
// 负责构建请求、设置 headers、发送请求、读取并返回 body bytes。
func fetchRawBody(ctx context.Context, httpClient *http.Client, copilotToken string, modelsURL string) ([]byte, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("创建 models 请求: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+copilotToken)
	req.Header.Set("Accept", "application/json")
	ApplyEditorHeaders(req)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("发送 models 请求: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取 models 响应: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models 请求失败: status=%d, body=%s", resp.StatusCode, string(body))
	}

	return body, nil
}

// FetchModelsFromAPI 从 Copilot API 获取当前账户实际可用的模型列表。
// 需要一个有效的 Copilot access token（非 OAuth token）。
// 自动适配 Individual（models 数组）和 Business（data 数组）响应格式。
// 只返回 model_picker_enabled=true、capabilities.type="chat" 的模型。
func FetchModelsFromAPI(ctx context.Context, httpClient *http.Client, copilotToken string, modelsURL string) ([]ModelInfo, error) {
	if modelsURL == "" {
		modelsURL = CopilotModelsURL
	}

	body, err := fetchRawBody(ctx, httpClient, copilotToken, modelsURL)
	if err != nil {
		return nil, err
	}

	// 尝试结构化解析（支持 models / data / 直接数组三种格式）
	var entries []copilotModelEntry

	var apiResp copilotModelsAPIResponse
	if err := json.Unmarshal(body, &apiResp); err == nil {
		if len(apiResp.Data) > 0 {
			entries = apiResp.Data // Business 格式
		} else if len(apiResp.Models) > 0 {
			entries = apiResp.Models // Individual 格式
		}
	}

	// 兜底：可能直接是数组
	if len(entries) == 0 {
		if err2 := json.Unmarshal(body, &entries); err2 != nil {
			return nil, fmt.Errorf("解析 models 响应: %w", err2)
		}
	}

	// 过滤：只保留 model_picker_enabled=true 且 capabilities.type="chat" 的模型
	var models []ModelInfo
	for _, entry := range entries {
		// 跳过非 picker 模型
		if !entry.ModelPickerEnabled {
			continue
		}
		// 跳过非 chat 类型（如 embeddings）
		if entry.Capabilities.Type != "" && entry.Capabilities.Type != "chat" {
			continue
		}

		name := entry.ID
		if name == "" {
			name = entry.Name
		}
		if name == "" {
			continue
		}

		multiplier := GetMultiplier(name)
		models = append(models, ModelInfo{
			Name:       name,
			Multiplier: multiplier,
			Category:   classifyModel(multiplier),
		})
	}

	// 按 Category 优先级排序
	sortModelInfoByCategory(models)

	return models, nil
}

// ===== CopilotModelDetail：全量上游元数据 =====

// CopilotModelDetail 完整的上游模型元数据。
type CopilotModelDetail struct {
	// 基础信息
	ID                  string   `json:"id"`
	Name                string   `json:"name"`
	Version             string   `json:"version"`
	Vendor              string   `json:"vendor"`
	Family              string   `json:"family"`
	Preview             bool     `json:"preview"`
	ModelPickerEnabled  bool     `json:"model_picker_enabled"`
	ModelPickerCategory string   `json:"model_picker_category,omitempty"`
	SupportedEndpoints  []string `json:"supported_endpoints,omitempty"`

	// 能力类型
	Type      string `json:"type"`      // chat, embeddings
	Tokenizer string `json:"tokenizer"` // o200k_base, cl100k_base

	// 上下文限制
	MaxContextWindowTokens int `json:"max_context_window_tokens,omitempty"`
	MaxOutputTokens        int `json:"max_output_tokens,omitempty"`
	MaxPromptTokens        int `json:"max_prompt_tokens,omitempty"`

	// 视觉能力
	Vision *VisionLimits `json:"vision,omitempty"`

	// 功能支持
	Supports *ModelSupports `json:"supports,omitempty"`

	// 额度信息（来自本地乘数表）
	Multiplier float64 `json:"multiplier"`
	Category   string  `json:"category"` // 免费/低消耗/标准/高消耗

	// Policy
	PolicyState string `json:"policy_state,omitempty"` // enabled/disabled
}

// VisionLimits 描述模型的视觉能力限制。
type VisionLimits struct {
	MaxPromptImageSize  int      `json:"max_prompt_image_size,omitempty"`
	MaxPromptImages     int      `json:"max_prompt_images,omitempty"`
	SupportedMediaTypes []string `json:"supported_media_types,omitempty"`
}

// ModelSupports 描述模型支持的功能特性。
type ModelSupports struct {
	Streaming         bool     `json:"streaming"`
	ToolCalls         bool     `json:"tool_calls"`
	ParallelToolCalls bool     `json:"parallel_tool_calls"`
	Vision            bool     `json:"vision"`
	StructuredOutputs bool     `json:"structured_outputs"`
	ReasoningEffort   []string `json:"reasoning_effort,omitempty"`
	AdaptiveThinking  bool     `json:"adaptive_thinking,omitempty"`
	MaxThinkingBudget int      `json:"max_thinking_budget,omitempty"`
	MinThinkingBudget int      `json:"min_thinking_budget,omitempty"`
}

// copilotModelDetailEntry 内部解析结构，用于从上游 API 解析全量字段。
type copilotModelDetailEntry struct {
	ID                  string   `json:"id"`
	Name                string   `json:"name"`
	Version             string   `json:"version"`
	Vendor              string   `json:"vendor"`
	Preview             bool     `json:"preview"`
	ModelPickerEnabled  bool     `json:"model_picker_enabled"`
	ModelPickerCategory string   `json:"model_picker_category"`
	SupportedEndpoints  []string `json:"supported_endpoints"`

	Policy *struct {
		State string `json:"state"`
	} `json:"policy,omitempty"`

	Capabilities struct {
		Family    string `json:"family"`
		Type      string `json:"type"`
		Tokenizer string `json:"tokenizer"`

		Limits struct {
			MaxContextWindowTokens int `json:"max_context_window_tokens"`
			MaxOutputTokens        int `json:"max_output_tokens"`
			MaxPromptTokens        int `json:"max_prompt_tokens"`
			Vision                 *struct {
				MaxPromptImageSize  int      `json:"max_prompt_image_size"`
				MaxPromptImages     int      `json:"max_prompt_images"`
				SupportedMediaTypes []string `json:"supported_media_types"`
			} `json:"vision,omitempty"`
		} `json:"limits"`

		Supports struct {
			Streaming         bool     `json:"streaming"`
			ToolCalls         bool     `json:"tool_calls"`
			ParallelToolCalls bool     `json:"parallel_tool_calls"`
			Vision            bool     `json:"vision"`
			StructuredOutputs bool     `json:"structured_outputs"`
			ReasoningEffort   []string `json:"reasoning_effort"`
			AdaptiveThinking  bool     `json:"adaptive_thinking"`
			MaxThinkingBudget int      `json:"max_thinking_budget"`
			MinThinkingBudget int      `json:"min_thinking_budget"`
		} `json:"supports"`
	} `json:"capabilities"`
}

// copilotModelsDetailAPIResponse 用于解析包含全量字段的 models API 响应。
type copilotModelsDetailAPIResponse struct {
	Models []copilotModelDetailEntry `json:"models"`
	Data   []copilotModelDetailEntry `json:"data"`
	Object string                    `json:"object"`
}

// FetchModelDetails 从 Copilot API 获取当前账户可用模型的完整元数据。
// 需要一个有效的 Copilot access token。
// 自动适配 Individual（models 数组）和 Business（data 数组）响应格式。
// 只返回 model_picker_enabled=true 且 capabilities.type="chat" 的模型。
// Multiplier 和 Category 从本地 ModelMultipliers 映射。
func FetchModelDetails(ctx context.Context, httpClient *http.Client, copilotToken string, modelsURL string) ([]CopilotModelDetail, error) {
	if modelsURL == "" {
		modelsURL = CopilotModelsURL
	}

	body, err := fetchRawBody(ctx, httpClient, copilotToken, modelsURL)
	if err != nil {
		return nil, err
	}

	// 尝试结构化解析（支持 models / data / 直接数组三种格式）
	var entries []copilotModelDetailEntry

	var apiResp copilotModelsDetailAPIResponse
	if err := json.Unmarshal(body, &apiResp); err == nil {
		if len(apiResp.Data) > 0 {
			entries = apiResp.Data // Business 格式
		} else if len(apiResp.Models) > 0 {
			entries = apiResp.Models // Individual 格式
		}
	}

	// 兜底：可能直接是数组
	if len(entries) == 0 {
		if err2 := json.Unmarshal(body, &entries); err2 != nil {
			return nil, fmt.Errorf("解析 models 响应: %w", err2)
		}
	}

	// 过滤并转换
	var models []CopilotModelDetail
	for _, entry := range entries {
		if !entry.ModelPickerEnabled {
			continue
		}
		if entry.Capabilities.Type != "" && entry.Capabilities.Type != "chat" {
			continue
		}

		name := entry.ID
		if name == "" {
			name = entry.Name
		}
		if name == "" {
			continue
		}

		multiplier := GetMultiplier(name)
		detail := CopilotModelDetail{
			ID:                     entry.ID,
			Name:                   entry.Name,
			Version:                entry.Version,
			Vendor:                 entry.Vendor,
			Family:                 entry.Capabilities.Family,
			Preview:                entry.Preview,
			ModelPickerEnabled:     entry.ModelPickerEnabled,
			ModelPickerCategory:    entry.ModelPickerCategory,
			SupportedEndpoints:     entry.SupportedEndpoints,
			Type:                   entry.Capabilities.Type,
			Tokenizer:              entry.Capabilities.Tokenizer,
			MaxContextWindowTokens: entry.Capabilities.Limits.MaxContextWindowTokens,
			MaxOutputTokens:        entry.Capabilities.Limits.MaxOutputTokens,
			MaxPromptTokens:        entry.Capabilities.Limits.MaxPromptTokens,
			Multiplier:             multiplier,
			Category:               classifyModel(multiplier),
		}

		// Vision limits
		if entry.Capabilities.Limits.Vision != nil {
			detail.Vision = &VisionLimits{
				MaxPromptImageSize:  entry.Capabilities.Limits.Vision.MaxPromptImageSize,
				MaxPromptImages:     entry.Capabilities.Limits.Vision.MaxPromptImages,
				SupportedMediaTypes: entry.Capabilities.Limits.Vision.SupportedMediaTypes,
			}
		}

		// Supports
		sup := entry.Capabilities.Supports
		detail.Supports = &ModelSupports{
			Streaming:         sup.Streaming,
			ToolCalls:         sup.ToolCalls,
			ParallelToolCalls: sup.ParallelToolCalls,
			Vision:            sup.Vision,
			StructuredOutputs: sup.StructuredOutputs,
			ReasoningEffort:   sup.ReasoningEffort,
			AdaptiveThinking:  sup.AdaptiveThinking,
			MaxThinkingBudget: sup.MaxThinkingBudget,
			MinThinkingBudget: sup.MinThinkingBudget,
		}

		// Policy
		if entry.Policy != nil {
			detail.PolicyState = entry.Policy.State
		}

		models = append(models, detail)
	}

	// 按 Category 优先级排序
	SortModelDetailByCategory(models)

	return models, nil
}

// classifyModel 根据乘数分类模型。
func classifyModel(multiplier float64) string {
	switch {
	case multiplier == 0:
		return "免费"
	case multiplier < 1:
		return "低消耗"
	case multiplier == 1:
		return "标准"
	default:
		return "高消耗"
	}
}
