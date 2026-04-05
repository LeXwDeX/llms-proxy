// models.go — 本地模型列表（拦截 /v1/models 等端点）。
package proxy

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/ycgame/llms-proxy/internal/auth"
	appmiddleware "github.com/ycgame/llms-proxy/internal/middleware"
)

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

	items := s.buildLocalDeployments(targetFilter)
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

func (s *Service) buildLocalDeployments(targetFilter map[string]struct{}) []map[string]any {
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
			result = append(result, map[string]any{
				"object":          "deployment",
				"id":              m,
				"model":           m,
				"status":          "succeeded",
				"created_at":      time.Now().Unix(),
				"deployed_tokens": nil,
			})
		}
	}
	// deterministic order
	sort.Slice(result, func(i, j int) bool {
		return strings.ToLower(result[i]["id"].(string)) < strings.ToLower(result[j]["id"].(string))
	})
	return result
}
