package config

import "strings"

// EndpointTypeMeta 是 endpoint_type 的单一信息源（Single Source of Truth）。
//
// 凡是涉及 endpoint_type 的「常量化业务数据」（机器名、UI 显示名、缩写、
// 徽章配色、是否需要 resource_path_prefix 等），全部在此声明，
// 由 admin API（GET /admin/data/endpoint-types）统一暴露给前端 UI 与下游消费方。
//
// 严禁在 admin/ui/index.html、handler、service 等其他层重复硬编码同一份元数据。
// 详见 .memory/architecture/single-source-of-truth-20260424.md
type EndpointTypeMeta struct {
	Code                       string `json:"code"`                          // 机器名
	DisplayName                string `json:"display_name"`                  // UI 长名（下拉、徽章）
	ShortLabel                 string `json:"short_label"`                   // UI 缩写（客户端列表等紧凑场景）
	BadgeBackground            string `json:"badge_background"`              // CSS 背景色
	BadgeForeground            string `json:"badge_foreground"`              // CSS 前景色
	RequiresResourcePathPrefix bool   `json:"requires_resource_path_prefix"` // 是否需要 resource_path_prefix（仅 azure_openai）
	SupportsBearerPassthrough  bool   `json:"supports_bearer_passthrough"`   // 是否支持 allow_bearer_passthrough（仅 azure_openai）
}

// endpointTypes 是受支持的 endpoint_type 全集。
//
// 新增 endpoint_type 时仅需在此追加一条；下游代码（IsValidEndpointType、
// admin API、UI 下拉等）会自动派生，无需逐处修改。
var endpointTypes = []EndpointTypeMeta{
	{
		Code: EndpointTypeAzureOpenAI, DisplayName: "Azure OpenAI", ShortLabel: "Azure",
		BadgeBackground: "#e0f2fe", BadgeForeground: "#0284c7",
		RequiresResourcePathPrefix: true, SupportsBearerPassthrough: true,
	},
	{
		Code: EndpointTypeOpenAI, DisplayName: "OpenAI", ShortLabel: "OpenAI",
		BadgeBackground: "#dbeafe", BadgeForeground: "#3b82f6",
	},
	{
		Code: EndpointTypeClaude, DisplayName: "Claude", ShortLabel: "Claude",
		BadgeBackground: "#fce7f3", BadgeForeground: "#db2777",
	},
	{
		Code: EndpointTypeGemini, DisplayName: "Gemini", ShortLabel: "Gemini",
		BadgeBackground: "#dcfce7", BadgeForeground: "#16a34a",
	},
	{
		Code: EndpointTypeWangsuOpenAI, DisplayName: "网宿 OpenAI", ShortLabel: "网宿OAI",
		BadgeBackground: "#ede9fe", BadgeForeground: "#7c3aed",
	},
	{
		Code: EndpointTypeWangsuOpenAIImage, DisplayName: "网宿 OpenAI 文生图", ShortLabel: "网宿图生",
		BadgeBackground: "#fef3c7", BadgeForeground: "#d97706",
	},
	{
		Code: EndpointTypeWangsuOpenAIImageEdit, DisplayName: "网宿 OpenAI 图编辑", ShortLabel: "网宿图编",
		BadgeBackground: "#ffedd5", BadgeForeground: "#ea580c",
	},
	{
		Code: EndpointTypeWangsuClaude, DisplayName: "网宿 Claude", ShortLabel: "网宿Claude",
		BadgeBackground: "#fdf4ff", BadgeForeground: "#a21caf",
	},
	{
		Code: EndpointTypeWangsuGemini, DisplayName: "网宿 Gemini", ShortLabel: "网宿Gemini",
		BadgeBackground: "#f0fdf4", BadgeForeground: "#15803d",
	},
	{
		Code: EndpointTypeCopilot, DisplayName: "Copilot", ShortLabel: "Copilot",
		BadgeBackground: "#f1f5f9", BadgeForeground: "#475569",
	},
	{
		Code: EndpointTypeDeepSeek, DisplayName: "DeepSeek", ShortLabel: "DeepSeek",
		BadgeBackground: "#eef2ff", BadgeForeground: "#4f46e5",
	},
}

// AllEndpointTypeMetas returns a copy of all registered endpoint type metadata
// in declaration order. UI / admin API consumers should use this as the
// single source of truth for rendering dropdowns and badges.
func AllEndpointTypeMetas() []EndpointTypeMeta {
	out := make([]EndpointTypeMeta, len(endpointTypes))
	copy(out, endpointTypes)
	return out
}

// EndpointTypeMetaOf returns the metadata for a given endpoint type code.
// The lookup is case-insensitive and trims whitespace, matching
// NormalizeEndpointType semantics.
func EndpointTypeMetaOf(code string) (EndpointTypeMeta, bool) {
	code = strings.ToLower(strings.TrimSpace(code))
	for _, m := range endpointTypes {
		if m.Code == code {
			return m, true
		}
	}
	return EndpointTypeMeta{}, false
}
