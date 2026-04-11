// models.go — 本地模型列表（拦截 /v1/models 等端点）。
package proxy

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ycgame/llms-proxy/internal/auth"
	"github.com/ycgame/llms-proxy/internal/catalog"
	"github.com/ycgame/llms-proxy/internal/copilot"
	appmiddleware "github.com/ycgame/llms-proxy/internal/middleware"
)

// localCatalog 是包级别的模型目录单例，用于为非 Copilot 模型补充属性。
var (
	localCatalogOnce sync.Once
	localCatalog     *catalog.Catalog
)

func getLocalCatalog() *catalog.Catalog {
	localCatalogOnce.Do(func() {
		c, err := catalog.New()
		if err != nil {
			slog.Warn("加载本地模型目录失败，模型属性将不可用", "error", err)
			return
		}
		localCatalog = c
	})
	return localCatalog
}

func isListEndpoint(path string) bool {
	path = strings.ToLower(path)
	return path == "/openai/deployments" ||
		path == "/openai/models" ||
		path == "/models" ||
		path == "/v1/models"
}

func (s *Service) maybeHandleLocalList(w http.ResponseWriter, r *http.Request) bool {
	if r == nil || r.Method != http.MethodGet {
		return false
	}
	path := strings.ToLower(r.URL.Path)
	if !isListEndpoint(path) {
		return false
	}

	principal, ok := auth.PrincipalFromContext(r.Context())
	if !ok || principal == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return true
	}

	var targetFilter map[string]struct{}
	if !principal.AllowAll() {
		targetFilter = normalizeAllowed(principal.AllowedTargets())
	}

	requested := strings.TrimSpace(r.Header.Get(headerProxyTarget))
	if requested == "" {
		requested = strings.TrimSpace(r.URL.Query().Get("target"))
	}
	if requested != "" {
		requestedLower := strings.ToLower(requested)
		if !principal.CanAccess(requestedLower) {
			http.Error(w, "target not allowed", http.StatusForbidden)
			return true
		}
		if _, exists := s.targetByName(requestedLower); !exists {
			http.Error(w, "unknown target", http.StatusBadRequest)
			return true
		}
		targetFilter = map[string]struct{}{requestedLower: {}}
	}

	items := s.buildLocalDeployments(r.Context(), targetFilter)
	resp := map[string]any{
		"object": "list",
		"data":   items,
		"first_id": func() string {
			if len(items) == 0 {
				return ""
			}
			if id, ok := items[0]["id"].(string); ok {
				return id
			}
			return ""
		}(),
		"last_id": func() string {
			if len(items) == 0 {
				return ""
			}
			if id, ok := items[len(items)-1]["id"].(string); ok {
				return id
			}
			return ""
		}(),
		"has_more": false,
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		s.logger.Warn("failed to encode local list response",
			"request_id", appmiddleware.RequestIDFromContext(r.Context()),
			"error", err,
		)
		http.Error(w, "failed to build response", http.StatusInternalServerError)
		return true
	}
	return true
}

func (s *Service) buildLocalDeployments(ctx context.Context, targetFilter map[string]struct{}) []map[string]any {
	seen := make(map[string]struct{})
	result := make([]map[string]any, 0)
	snapshot := s.targetSnapshot()
	for _, state := range snapshot {
		if state == nil || state.Target() == nil {
			continue
		}
		target := state.Target()
		if targetFilter != nil {
			targetKey := strings.ToLower(strings.TrimSpace(target.Name))
			if _, allowed := targetFilter[targetKey]; !allowed {
				continue
			}
		}
		models := target.AllowedModels
		for _, m := range models {
			m = strings.TrimSpace(m)
			if m == "" {
				continue
			}
			key := strings.ToLower(m)
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			item := map[string]any{
				"object":   "model",
				"id":       m,
				"model":    m,
				"created":  time.Now().Unix(),
				"owned_by": "target:" + strings.ToLower(strings.TrimSpace(target.Name)),
			}

			// 从 catalog 查询模型属性，补充 context_length / max_output_tokens / capabilities
			if cat := getLocalCatalog(); cat != nil {
				if entry := cat.Lookup(target.EndpointType, m); entry != nil {
					if entry.ContextWindow > 0 {
						item["context_length"] = entry.ContextWindow
					}
					if entry.MaxOutputTokens > 0 {
						item["max_output_tokens"] = entry.MaxOutputTokens
					}
					if len(entry.Capabilities) > 0 {
						item["capabilities"] = entry.Capabilities
					}
				}
			}

			result = append(result, item)
		}
	}

	// 注入 Copilot 模型
	if s.copilotService != nil {
		copilotModels, err := s.copilotService.GetCachedModels(ctx)
		if err != nil {
			s.logger.Debug("获取 Copilot 模型缓存失败，跳过注入", "error", err)
		} else {
			for _, cm := range copilotModels {
				modelID := copilot.ModelPrefix + cm.ID
				key := strings.ToLower(modelID)
				if _, exists := seen[key]; exists {
					continue
				}
				seen[key] = struct{}{}

				item := map[string]any{
					"id":       modelID,
					"object":   "model",
					"created":  time.Now().Unix(),
					"owned_by": "copilot:" + strings.ToLower(cm.Vendor),
					// 扩展字段
					"context_length":      cm.MaxContextWindowTokens,
					"max_output_tokens":   cm.MaxOutputTokens,
					"supported_endpoints": cm.SupportedEndpoints,
					"vendor":              cm.Vendor,
					"preview":             cm.Preview,
					"multiplier":          cm.Multiplier,
				}
				if cm.Supports != nil {
					item["capabilities"] = map[string]any{
						"vision":             cm.Supports.Vision,
						"tool_calls":         cm.Supports.ToolCalls,
						"streaming":          cm.Supports.Streaming,
						"reasoning_effort":   cm.Supports.ReasoningEffort,
						"structured_outputs": cm.Supports.StructuredOutputs,
					}
				}
				result = append(result, item)
			}
		}
	}

	// deterministic order
	sort.Slice(result, func(i, j int) bool {
		return strings.ToLower(result[i]["id"].(string)) < strings.ToLower(result[j]["id"].(string))
	})
	return result
}
