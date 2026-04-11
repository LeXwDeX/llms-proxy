package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"log/slog"

	"github.com/ycgame/llms-proxy/internal/auth"
	"github.com/ycgame/llms-proxy/internal/catalog"
	"github.com/ycgame/llms-proxy/internal/config"
	"github.com/ycgame/llms-proxy/internal/copilot"
	"github.com/ycgame/llms-proxy/internal/nosql"
	"github.com/ycgame/llms-proxy/internal/proxy"
	"github.com/ycgame/llms-proxy/internal/usage"
)

// Handler wires administration endpoints.
type Handler struct {
	configManager    *config.Manager
	authStore        *auth.Store
	proxyService     *proxy.Service
	auditStore       *nosql.AuditStore
	userStore        *nosql.UserStore
	clientStore      *nosql.ClientStore
	modelCostStore   *nosql.ModelCostStore
	usageStore       *nosql.UsageStore
	modelCatalog     *catalog.Catalog
	copilotPoolStore *nosql.CopilotPoolStore
	copilotService   *copilot.CopilotService    // nil = copilot 未配置
	copilotAcctStore *nosql.CopilotAccountStore // nil = copilot 未配置
	copilotQuotaMgr  *copilot.QuotaManager      // nil = copilot 未配置
	logger           *slog.Logger
}

// NewHandler constructs an HTTP router exposing admin endpoints.
func NewHandler(
	manager *config.Manager,
	store *auth.Store,
	service *proxy.Service,
	auditStore *nosql.AuditStore,
	userStore *nosql.UserStore,
	clientStore *nosql.ClientStore,
	modelCostStore *nosql.ModelCostStore,
	usageStore *nosql.UsageStore,
	modelCatalog *catalog.Catalog,
	copilotPoolStore *nosql.CopilotPoolStore,
	copilotService *copilot.CopilotService,
	copilotAcctStore *nosql.CopilotAccountStore,
	copilotQuotaMgr *copilot.QuotaManager,
	logger *slog.Logger,
) http.Handler {
	h := &Handler{
		configManager:    manager,
		authStore:        store,
		proxyService:     service,
		auditStore:       auditStore,
		userStore:        userStore,
		clientStore:      clientStore,
		modelCostStore:   modelCostStore,
		usageStore:       usageStore,
		modelCatalog:     modelCatalog,
		copilotPoolStore: copilotPoolStore,
		copilotService:   copilotService,
		copilotAcctStore: copilotAcctStore,
		copilotQuotaMgr:  copilotQuotaMgr,
		logger:           logger,
	}

	r := chi.NewRouter()
	r.Get("/", h.handleUI)
	r.Get("/ui", h.handleUI)
	r.Get("/healthz", h.handleHealthz)
	r.Get("/metrics", h.handleMetrics)
	r.Get("/api/me", h.handleMe)
	r.Get("/api/overview", h.handleOverview)
	r.Post("/api/change-password", h.handleChangePassword)
	r.Post("/config/reload", h.handleReloadConfig)
	r.Route("/data", func(r chi.Router) {
		r.Get("/clients", h.handleListClients)
		r.Post("/clients", h.handleCreateClient)
		r.Put("/clients/{name}", h.handleUpdateClient)
		r.Delete("/clients/{name}", h.handleDeleteClient)

		r.Get("/model-costs", h.handleListModelCosts)
		r.Put("/model-costs/{model}", h.handleUpsertModelCost)
		r.Delete("/model-costs/{model}", h.handleDeleteModelCost)

		r.Get("/usage/events", h.handleListUsageEvents)
		r.Get("/usage/aggregate", h.handleAggregateUsage)
		r.Get("/usage/summary", h.handleUsageSummary)
		r.Get("/audit", h.handleListAudit)

		// Target management
		r.Get("/targets", h.handleListTargets)
		r.Post("/targets", h.handleCreateTarget)
		r.Put("/targets/{name}", h.handleUpdateTarget)
		r.Delete("/targets/{name}", h.handleDeleteTarget)

		// Copilot pool management
		r.Get("/copilot-pools", h.handleListCopilotPools)
		r.Post("/copilot-pools", h.handleCreateCopilotPool)
		r.Put("/copilot-pools/{name}", h.handleUpdateCopilotPool)
		r.Delete("/copilot-pools/{name}", h.handleDeleteCopilotPool)

		// Copilot account management
		r.Get("/copilot-accounts", h.handleListCopilotAccounts)
		r.Get("/copilot-accounts/{id}", h.handleGetCopilotAccount)
		r.Post("/copilot-accounts/auth/start", h.handleStartCopilotAuth)
		r.Post("/copilot-accounts/auth/complete/{id}", h.handleCompleteCopilotAuth)
		r.Post("/copilot-accounts/{id}/revoke", h.handleRevokeCopilotAccount)
		r.Post("/copilot-accounts/{id}/disable", h.handleDisableCopilotAccount)
		r.Post("/copilot-accounts/{id}/enable", h.handleEnableCopilotAccount)
		r.Delete("/copilot-accounts/{id}", h.handleDeleteCopilotAccount)
		r.Get("/copilot-accounts/{id}/quota", h.handleGetCopilotQuota)
		r.Post("/copilot-accounts/{id}/quota/sync", h.handleSyncCopilotQuota)
		r.Get("/copilot-models", h.handleListCopilotModels)

		// Model catalog
		r.Get("/catalog", h.handleListCatalog)
		r.Get("/catalog/{endpoint_type}", h.handleListCatalogByType)
	})
	return r
}

func (h *Handler) handleHealthz(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	status := h.proxyService.TargetStatuses(now)
	response := struct {
		Status    string               `json:"status"`
		CheckedAt time.Time            `json:"checked_at"`
		Targets   []proxy.TargetStatus `json:"targets"`
		Count     int                  `json:"target_count"`
		Muted     int                  `json:"muted_targets"`
	}{
		Status:    "ok",
		CheckedAt: now,
		Targets:   status,
		Count:     len(status),
	}

	for _, target := range status {
		if target.Muted {
			response.Muted++
		}
		if target.Muted && response.Status == "ok" {
			response.Status = "degraded"
		}
	}

	writeJSON(w, http.StatusOK, response)
}

func (h *Handler) handleMetrics(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	metrics := h.proxyService.MetricsSnapshot()
	uptime := now.Sub(metrics.StartTime)
	if uptime < 0 {
		uptime = 0
	}

	response := struct {
		GeneratedAt    time.Time `json:"generated_at"`
		UptimeSeconds  float64   `json:"uptime_seconds"`
		ActiveRequests int64     `json:"active_requests"`
		Requests       struct {
			Total    int64 `json:"total"`
			Success  int64 `json:"success"`
			Failures int64 `json:"failures"`
			Retries  int64 `json:"retries"`
		} `json:"requests"`
		Targets int `json:"targets"`
	}{
		GeneratedAt:    now,
		UptimeSeconds:  uptime.Seconds(),
		ActiveRequests: metrics.ActiveRequests,
		Targets:        len(h.proxyService.TargetStatuses(now)),
	}
	response.Requests.Total = metrics.TotalRequests
	response.Requests.Success = metrics.TotalSuccess
	response.Requests.Failures = metrics.TotalFailures
	response.Requests.Retries = metrics.TotalRetries

	writeJSON(w, http.StatusOK, response)
}

func (h *Handler) handleMe(w http.ResponseWriter, r *http.Request) {
	if session, ok := SessionFromContext(r.Context()); ok && session != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"authenticated": true,
			"username":      session.Username,
			"role":          session.Role,
			"expires_at":    session.ExpiresAt,
		})
		return
	}
	writeJSON(w, http.StatusUnauthorized, map[string]any{"authenticated": false})
}

func (h *Handler) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	session, ok := SessionFromContext(r.Context())
	if !ok || session == nil {
		writeJSON(w, http.StatusUnauthorized, errorResponse("not authenticated"))
		return
	}

	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid request body"))
		return
	}

	if strings.TrimSpace(req.OldPassword) == "" || strings.TrimSpace(req.NewPassword) == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("旧密码和新密码均不能为空"))
		return
	}
	if len(req.NewPassword) < 6 {
		writeJSON(w, http.StatusBadRequest, errorResponse("新密码至少需要 6 个字符"))
		return
	}

	// Verify old password.
	if _, err := AuthenticateUser(h.userStore, session.Username, req.OldPassword); err != nil {
		writeJSON(w, http.StatusForbidden, errorResponse("原密码验证失败"))
		return
	}

	// Generate new hash with random salt.
	newHash, err := HashPasswordWithRandomSalt(req.NewPassword)
	if err != nil {
		h.writeInternalError(w, "failed to hash password", err)
		return
	}

	// Update the user's password hash via nosql store.
	user, err := h.userStore.Get(session.Username)
	if err != nil {
		h.writeInternalError(w, "failed to get user", err)
		return
	}
	user.PasswordHash = newHash
	if err := h.userStore.Update(session.Username, user); err != nil {
		h.writeInternalError(w, "failed to update password", err)
		return
	}

	h.recordAudit(r, "change_password", session.Username, "success", "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "密码已修改"})
}

func (h *Handler) handleOverview(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	health := h.proxyService.TargetStatuses(now)
	metrics := h.proxyService.MetricsSnapshot()

	clients, err := h.clientStore.List()
	if err != nil {
		h.writeInternalError(w, "failed to list clients", err)
		return
	}
	costs, err := h.modelCostStore.List()
	if err != nil {
		h.writeInternalError(w, "failed to list model costs", err)
		return
	}
	summary, err := h.usageStore.Summary(now, toUsageCostTable(costs, h.modelCatalog))
	if err != nil {
		h.writeInternalError(w, "failed to summarize usage", err)
		return
	}

	activeTargets := 0
	for _, t := range health {
		if !t.Muted {
			activeTargets++
		}
	}

	// 72-hour request/success stats from usage events.
	from72h := now.Add(-72 * time.Hour)
	reqs72h, success72h, _ := h.usageStore.Count(from72h, now)
	var successRate72h float64
	if reqs72h > 0 {
		successRate72h = float64(success72h) / float64(reqs72h) * 100
	}

	response := map[string]any{
		"generated_at":     now,
		"targets":          health,
		"target_count":     len(health),
		"active_targets":   activeTargets,
		"active_requests":  metrics.ActiveRequests,
		"total_requests":   metrics.TotalRequests,
		"success_rate":     successRate72h,
		"requests_72h":     reqs72h,
		"success_72h":      success72h,
		"client_count":     len(clients),
		"model_cost_count": len(costs),
		"requests": map[string]int64{
			"total":    metrics.TotalRequests,
			"success":  metrics.TotalSuccess,
			"failures": metrics.TotalFailures,
			"retries":  metrics.TotalRetries,
		},
		"usage_summary": summary,
	}
	if events, err := h.listAuditEvents(10); err == nil {
		response["recent_audit"] = events
	}
	writeJSON(w, http.StatusOK, response)
}

func (h *Handler) handleListAudit(w http.ResponseWriter, r *http.Request) {
	events, err := h.listAuditEventsFromRequest(r)
	if err != nil {
		h.writeInternalError(w, "failed to list audit events", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"events": events,
		"count":  len(events),
	})
}

func (h *Handler) handleReloadConfig(w http.ResponseWriter, r *http.Request) {
	path := h.configManager.Path()
	prev, _ := h.configManager.Current()

	cfg, err := config.Load(path)
	if err != nil {
		h.logger.Error("admin reload failed: load config",
			"path", path,
			"error", err,
		)
		writeJSON(w, http.StatusInternalServerError, errorResponse("failed to load config"))
		return
	}

	// Use the existing clientStore (backed by bbolt, not file path).
	tempStore := auth.NewStore()
	clients, err := h.clientStore.List()
	if err != nil {
		h.logger.Warn("admin reload rejected: invalid clients",
			"error", err,
		)
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid clients"))
		return
	}

	if err := tempStore.LoadFromConfig(clients); err != nil {
		h.logger.Warn("admin reload rejected: invalid clients",
			"error", err,
		)
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid client configuration"))
		return
	}

	if err := h.proxyService.ApplyConfig(cfg); err != nil {
		h.logger.Warn("admin reload rejected: invalid proxy configuration",
			"error", err,
		)
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid proxy configuration"))
		return
	}

	if err := h.authStore.LoadFromConfig(clients); err != nil {
		h.logger.Error("admin reload failed: auth apply",
			"error", err,
		)
		if prev != nil {
			if revertErr := h.proxyService.ApplyConfig(prev); revertErr != nil {
				h.logger.Error("admin reload revert failed", "error", revertErr)
			}
		}
		writeJSON(w, http.StatusInternalServerError, errorResponse("failed to update auth configuration"))
		return
	}

	h.configManager.Replace(cfg)
	h.recordAudit(r, "config_reload", path, "success", fmt.Sprintf("targets=%d clients=%d", len(cfg.Targets), len(clients)))
	h.logger.Info("configuration reloaded via admin endpoint",
		"path", path,
		"targets", len(cfg.Targets),
		"clients", len(clients),
	)

	response := struct {
		Status     string    `json:"status"`
		ReloadedAt time.Time `json:"reloaded_at"`
		Targets    int       `json:"targets"`
		Clients    int       `json:"clients"`
	}{
		Status:     "ok",
		ReloadedAt: time.Now(),
		Targets:    len(cfg.Targets),
		Clients:    len(clients),
	}

	writeJSON(w, http.StatusOK, response)
}

func (h *Handler) handleListClients(w http.ResponseWriter, r *http.Request) {
	clients, err := h.clientStore.List()
	if err != nil {
		h.writeInternalError(w, "failed to list clients", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"clients": clients,
		"count":   len(clients),
	})
}

func (h *Handler) handleCreateClient(w http.ResponseWriter, r *http.Request) {
	var req config.Client
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid json body"))
		return
	}

	if err := h.clientStore.Create(req); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "already exists") {
			status = http.StatusConflict
		}
		writeJSON(w, status, errorResponse(err.Error()))
		return
	}

	if err := h.reloadAuthFromClientStore(); err != nil {
		h.writeInternalError(w, "failed to apply auth store", err)
		return
	}
	h.recordAudit(r, "client_create", req.Name, "success", maskKey(req.AccessKey))

	writeJSON(w, http.StatusCreated, map[string]string{"status": "ok"})
}

func (h *Handler) handleUpdateClient(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if strings.TrimSpace(name) == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("missing client name"))
		return
	}

	var req config.Client
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid json body"))
		return
	}

	if err := h.clientStore.Update(name, req); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		} else if strings.Contains(err.Error(), "already exists") {
			status = http.StatusConflict
		}
		writeJSON(w, status, errorResponse(err.Error()))
		return
	}

	if err := h.reloadAuthFromClientStore(); err != nil {
		h.writeInternalError(w, "failed to apply auth store", err)
		return
	}
	h.recordAudit(r, "client_update", name, "success", maskKey(req.AccessKey))

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) handleDeleteClient(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if strings.TrimSpace(name) == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("missing client name"))
		return
	}

	if err := h.clientStore.Delete(name); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		writeJSON(w, status, errorResponse(err.Error()))
		return
	}

	if err := h.reloadAuthFromClientStore(); err != nil {
		h.writeInternalError(w, "failed to apply auth store", err)
		return
	}
	h.recordAudit(r, "client_delete", name, "success", "")

	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleListModelCosts(w http.ResponseWriter, r *http.Request) {
	models, err := h.modelCostStore.List()
	if err != nil {
		h.writeInternalError(w, "failed to list model costs", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"models": models,
		"count":  len(models),
	})
}

func (h *Handler) handleUpsertModelCost(w http.ResponseWriter, r *http.Request) {
	model := strings.ToLower(strings.TrimSpace(chi.URLParam(r, "model")))
	if model == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("missing model"))
		return
	}

	var req struct {
		EndpointType          string  `json:"endpoint_type"`
		InputPer1MTokens      float64 `json:"input_per_1m_tokens"`
		OutputPer1MTokens     float64 `json:"output_per_1m_tokens"`
		CachedInputPer1MToken float64 `json:"cached_input_per_1m_tokens"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid json body"))
		return
	}

	// When the UI edits an existing record and the user changed the endpoint_type
	// dropdown, the original endpoint_type is passed as a query param so we can
	// delete the old (model, original_ep) record before writing the new one —
	// preventing orphaned duplicate entries.
	originalEpType := strings.TrimSpace(r.URL.Query().Get("endpoint_type"))
	newEpType := strings.ToLower(strings.TrimSpace(req.EndpointType))
	if newEpType == "" {
		newEpType = "azure_openai"
	}
	if originalEpType != "" && !strings.EqualFold(originalEpType, newEpType) {
		// Ignore deletion errors (record may not exist under original key).
		_ = h.modelCostStore.DeleteByKey(originalEpType, model)
	}

	cost := nosql.ModelCost{
		EndpointType:          req.EndpointType,
		Model:                 model,
		InputPer1MTokens:      req.InputPer1MTokens,
		OutputPer1MTokens:     req.OutputPer1MTokens,
		CachedInputPer1MToken: req.CachedInputPer1MToken,
	}
	if err := h.modelCostStore.Upsert(cost); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
		return
	}
	h.recordAudit(r, "model_cost_upsert", model, "success", "")

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"model":         model,
		"endpoint_type": cost.EndpointType,
	})
}

func (h *Handler) handleDeleteModelCost(w http.ResponseWriter, r *http.Request) {
	model := strings.TrimSpace(chi.URLParam(r, "model"))
	if model == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("missing model"))
		return
	}

	endpointType := r.URL.Query().Get("endpoint_type")
	var deleteErr error
	if endpointType != "" {
		deleteErr = h.modelCostStore.DeleteByKey(endpointType, model)
	} else {
		deleteErr = h.modelCostStore.Delete(model)
	}
	if deleteErr != nil {
		status := http.StatusBadRequest
		if strings.Contains(deleteErr.Error(), "not found") {
			status = http.StatusNotFound
		}
		writeJSON(w, status, errorResponse(deleteErr.Error()))
		return
	}
	h.recordAudit(r, "model_cost_delete", model, "success", "")

	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleListUsageEvents(w http.ResponseWriter, r *http.Request) {
	filter, err := parseUsageFilter(r, true)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
		return
	}

	events, err := h.usageStore.List(filter)
	if err != nil {
		h.writeInternalError(w, "failed to list usage events", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"events": events,
		"count":  len(events),
	})
}

func (h *Handler) handleAggregateUsage(w http.ResponseWriter, r *http.Request) {
	filter, err := parseUsageFilter(r, false)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
		return
	}
	costs, err := h.modelCostStore.List()
	if err != nil {
		h.writeInternalError(w, "failed to list model costs", err)
		return
	}

	result, err := h.usageStore.Aggregate(filter, r.URL.Query().Get("group_by"), toUsageCostTable(costs, h.modelCatalog))
	if err != nil {
		h.writeInternalError(w, "failed to aggregate usage", err)
		return
	}

	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) handleUsageSummary(w http.ResponseWriter, r *http.Request) {
	costs, err := h.modelCostStore.List()
	if err != nil {
		h.writeInternalError(w, "failed to list model costs", err)
		return
	}

	result, err := h.usageStore.Summary(time.Now().UTC(), toUsageCostTable(costs, h.modelCatalog))
	if err != nil {
		h.writeInternalError(w, "failed to summarize usage", err)
		return
	}

	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) handleUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	_, _ = w.Write([]byte(adminUIHTML))
}

// ===== Target CRUD =====

func (h *Handler) handleListTargets(w http.ResponseWriter, r *http.Request) {
	cfg, err := h.currentConfig()
	if err != nil {
		h.writeInternalError(w, "failed to load config", err)
		return
	}

	targets := make([]map[string]any, len(cfg.Targets))
	for i, t := range cfg.Targets {
		epType := config.NormalizeEndpointType(t.EndpointType)
		sseAutoAgg := t.SSEAutoAggregate == nil || *t.SSEAutoAggregate
		targets[i] = map[string]any{
			"name":                     t.Name,
			"endpoint_type":            epType,
			"endpoint":                 t.Endpoint,
			"resource_path_prefix":     t.ResourcePathPrefix,
			"has_api_key":              t.APIKey != "",
			"allow_bearer_passthrough": t.AllowBearer,
			"allowed_models":           t.AllowedModels,
			"sse_auto_aggregate":       sseAutoAgg,
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"targets": targets,
		"count":   len(targets),
	})
}

func (h *Handler) handleCreateTarget(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name               string   `json:"name"`
		EndpointType       string   `json:"endpoint_type"`
		Endpoint           string   `json:"endpoint"`
		ResourcePathPrefix string   `json:"resource_path_prefix"`
		APIKey             string   `json:"api_key"`
		AllowBearer        bool     `json:"allow_bearer_passthrough"`
		AllowedModels      []string `json:"allowed_models"`
		SSEAutoAggregate   *bool    `json:"sse_auto_aggregate,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid request body"))
		return
	}

	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("name must not be empty"))
		return
	}
	epType := config.NormalizeEndpointType(body.EndpointType)
	if !config.IsValidEndpointType(epType) {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid endpoint_type"))
		return
	}
	endpoint := strings.TrimSpace(body.Endpoint)
	if endpoint == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("endpoint must not be empty"))
		return
	}
	apiKey := strings.TrimSpace(body.APIKey)
	if apiKey == "" && !body.AllowBearer {
		writeJSON(w, http.StatusBadRequest, errorResponse("api_key must not be empty when allow_bearer_passthrough is false"))
		return
	}

	rpp := strings.TrimSpace(body.ResourcePathPrefix)
	if epType == config.EndpointTypeAzureOpenAI && rpp == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("resource_path_prefix is required for azure_openai targets"))
		return
	}

	cfg, err := h.currentConfig()
	if err != nil {
		h.writeInternalError(w, "failed to load config", err)
		return
	}

	for _, t := range cfg.Targets {
		if strings.EqualFold(t.Name, name) {
			writeJSON(w, http.StatusConflict, errorResponse(fmt.Sprintf("target %q already exists", name)))
			return
		}
	}

	newTarget := config.Target{
		Name:               name,
		EndpointType:       epType,
		Endpoint:           endpoint,
		ResourcePathPrefix: rpp,
		APIKey:             apiKey,
		AllowBearer:        body.AllowBearer,
		AllowedModels:      body.AllowedModels,
		SSEAutoAggregate:   body.SSEAutoAggregate,
	}
	cfg.Targets = append(cfg.Targets, newTarget)

	if err := h.applyConfigRuntime(cfg); err != nil {
		h.writeInternalError(w, "failed to apply config", err)
		return
	}

	if err := h.saveConfig(cfg); err != nil {
		h.logger.Error("runtime config applied but save to disk failed; changes will be lost on restart", "error", err)
		h.recordAudit(r, "create_target", name, "partial", "runtime applied but persist failed: "+err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"ok":    false,
			"error": "config applied at runtime but failed to persist to disk; changes will be lost on restart",
		})
		return
	}

	h.recordAudit(r, "create_target", name, "success", "")
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "name": name})
}

func (h *Handler) handleUpdateTarget(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if strings.TrimSpace(name) == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("name must not be empty"))
		return
	}

	var body struct {
		EndpointType       string   `json:"endpoint_type"`
		Endpoint           string   `json:"endpoint"`
		ResourcePathPrefix string   `json:"resource_path_prefix"`
		APIKey             *string  `json:"api_key"`
		AllowBearer        bool     `json:"allow_bearer_passthrough"`
		AllowedModels      []string `json:"allowed_models"`
		SSEAutoAggregate   *bool    `json:"sse_auto_aggregate,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid request body"))
		return
	}

	cfg, err := h.currentConfig()
	if err != nil {
		h.writeInternalError(w, "failed to load config", err)
		return
	}

	found := false
	for i := range cfg.Targets {
		if strings.EqualFold(cfg.Targets[i].Name, name) {
			t := &cfg.Targets[i]
			if body.EndpointType != "" {
				epType := config.NormalizeEndpointType(body.EndpointType)
				if !config.IsValidEndpointType(epType) {
					writeJSON(w, http.StatusBadRequest, errorResponse("invalid endpoint_type"))
					return
				}
				t.EndpointType = epType
			}
			if body.Endpoint != "" {
				t.Endpoint = strings.TrimSpace(body.Endpoint)
			}
			// Validate RPP requirement for azure_openai before writing.
			effectiveEpType := config.NormalizeEndpointType(t.EndpointType)
			rpp := strings.TrimSpace(body.ResourcePathPrefix)
			if effectiveEpType == config.EndpointTypeAzureOpenAI && rpp == "" {
				writeJSON(w, http.StatusBadRequest, errorResponse("resource_path_prefix is required for azure_openai targets"))
				return
			}
			t.ResourcePathPrefix = rpp
			if body.APIKey != nil {
				t.APIKey = strings.TrimSpace(*body.APIKey)
			}
			t.AllowBearer = body.AllowBearer
			t.AllowedModels = body.AllowedModels
			if body.SSEAutoAggregate != nil {
				t.SSEAutoAggregate = body.SSEAutoAggregate
			}

			// Validate: api_key must be non-empty when allow_bearer is false.
			if t.APIKey == "" && !t.AllowBearer {
				writeJSON(w, http.StatusBadRequest, errorResponse("api_key must not be empty when allow_bearer_passthrough is false"))
				return
			}
			found = true
			break
		}
	}

	if !found {
		writeJSON(w, http.StatusNotFound, errorResponse(fmt.Sprintf("target %q not found", name)))
		return
	}

	if err := h.applyConfigRuntime(cfg); err != nil {
		h.writeInternalError(w, "failed to apply config", err)
		return
	}

	if err := h.saveConfig(cfg); err != nil {
		h.logger.Error("runtime config applied but save to disk failed; changes will be lost on restart", "error", err)
		h.recordAudit(r, "update_target", name, "partial", "runtime applied but persist failed: "+err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"ok":    false,
			"error": "config applied at runtime but failed to persist to disk; changes will be lost on restart",
		})
		return
	}

	h.recordAudit(r, "update_target", name, "success", "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": name})
}

func (h *Handler) handleDeleteTarget(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if strings.TrimSpace(name) == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("name must not be empty"))
		return
	}

	cfg, err := h.currentConfig()
	if err != nil {
		h.writeInternalError(w, "failed to load config", err)
		return
	}

	next := make([]config.Target, 0, len(cfg.Targets))
	found := false
	for _, t := range cfg.Targets {
		if strings.EqualFold(t.Name, name) {
			found = true
			continue
		}
		next = append(next, t)
	}

	if !found {
		writeJSON(w, http.StatusNotFound, errorResponse(fmt.Sprintf("target %q not found", name)))
		return
	}

	cfg.Targets = next

	if err := h.applyConfigRuntime(cfg); err != nil {
		h.writeInternalError(w, "failed to apply config", err)
		return
	}

	if err := h.saveConfig(cfg); err != nil {
		h.logger.Error("runtime config applied but save to disk failed; changes will be lost on restart", "error", err)
		h.recordAudit(r, "delete_target", name, "partial", "runtime applied but persist failed: "+err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"ok":    false,
			"error": "config applied at runtime but failed to persist to disk; changes will be lost on restart",
		})
		return
	}

	h.recordAudit(r, "delete_target", name, "success", "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) saveConfig(cfg *config.Config) error {
	path := h.configManager.Path()
	payload, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	payload = append(payload, '\n')

	dir := filepath.Dir(path)
	tmp := filepath.Join(dir, fmt.Sprintf(".config.%s.tmp", uuid.NewString()))
	if err := os.WriteFile(tmp, payload, 0o644); err != nil {
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename config: %w", err)
	}

	h.configManager.Replace(cfg)
	return nil
}

func (h *Handler) applyConfigRuntime(cfg *config.Config) error {
	clients, err := h.clientStore.List()
	if err != nil {
		return fmt.Errorf("load clients: %w", err)
	}

	if err := h.proxyService.ApplyConfig(cfg); err != nil {
		return fmt.Errorf("apply proxy config: %w", err)
	}

	if err := h.authStore.LoadFromConfig(clients); err != nil {
		return fmt.Errorf("apply auth config: %w", err)
	}

	return nil
}

// ===== Catalog API =====

func (h *Handler) handleListCatalog(w http.ResponseWriter, r *http.Request) {
	if h.modelCatalog == nil {
		writeJSON(w, http.StatusOK, map[string]any{"models": []any{}, "count": 0})
		return
	}

	epType := r.URL.Query().Get("endpoint_type")
	var models []catalog.ModelEntry
	if epType != "" {
		models = h.modelCatalog.ListByEndpointType(epType)
	} else {
		models = h.modelCatalog.ListAll()
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"models": models,
		"count":  len(models),
	})
}

func (h *Handler) handleListCatalogByType(w http.ResponseWriter, r *http.Request) {
	if h.modelCatalog == nil {
		writeJSON(w, http.StatusOK, map[string]any{"models": []any{}, "count": 0})
		return
	}

	epType := chi.URLParam(r, "endpoint_type")
	models := h.modelCatalog.ListByEndpointType(epType)
	writeJSON(w, http.StatusOK, map[string]any{
		"models": models,
		"count":  len(models),
	})
}

func (h *Handler) currentConfig() (*config.Config, error) {
	cfg, err := h.configManager.Current()
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, errors.New("config unavailable")
	}
	return cfg, nil
}

func (h *Handler) reloadAuthFromClientStore() error {
	clients, err := h.clientStore.List()
	if err != nil {
		return err
	}
	return h.authStore.LoadFromConfig(clients)
}

func (h *Handler) writeInternalError(w http.ResponseWriter, message string, err error) {
	h.logger.Error(message, "error", err)
	writeJSON(w, http.StatusInternalServerError, errorResponse("internal server error"))
}

func (h *Handler) listAuditEvents(limit int) ([]nosql.AuditEvent, error) {
	if h.auditStore == nil {
		return []nosql.AuditEvent{}, nil
	}
	return h.auditStore.List(limit)
}

func (h *Handler) listAuditEventsFromRequest(r *http.Request) ([]nosql.AuditEvent, error) {
	limit := 50
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			return nil, errors.New("invalid limit")
		}
		limit = parsed
	}
	return h.listAuditEvents(limit)
}

func (h *Handler) recordAudit(r *http.Request, action, object, result, detail string) {
	if h.auditStore == nil {
		return
	}
	actor := "system"
	if session, ok := SessionFromContext(r.Context()); ok && session != nil && strings.TrimSpace(session.Username) != "" {
		actor = session.Username
	}
	_ = h.auditStore.Record(nosql.AuditEvent{
		Timestamp: time.Now().UTC(),
		Actor:     actor,
		Action:    action,
		Object:    object,
		Result:    result,
		Detail:    detail,
	})
}

func parseUsageFilter(r *http.Request, withLimit bool) (usage.Filter, error) {
	query := r.URL.Query()

	from, err := parseTimeValue(query.Get("from"))
	if err != nil {
		return usage.Filter{}, errors.New("invalid from, expect RFC3339")
	}
	to, err := parseTimeValue(query.Get("to"))
	if err != nil {
		return usage.Filter{}, errors.New("invalid to, expect RFC3339")
	}

	filter := usage.Filter{
		From:       from,
		To:         to,
		ClientName: strings.TrimSpace(query.Get("client_name")),
		Model:      strings.TrimSpace(query.Get("model")),
	}

	if withLimit {
		limit := 200
		if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed <= 0 {
				return usage.Filter{}, errors.New("invalid limit")
			}
			limit = parsed
		}
		filter.Limit = limit
	}

	return filter, nil
}

func parseTimeValue(raw string) (*time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, err
	}
	t = t.UTC()
	return &t, nil
}

// toUsageCostTable builds a cost lookup table.
// Priority: custom model_costs override > catalog embedded default prices.
func toUsageCostTable(costs []nosql.ModelCost, cat *catalog.Catalog) usage.CostTable {
	table := make(usage.CostTable)

	// Layer 1: Fill catalog default prices as baseline.
	if cat != nil {
		for _, entry := range cat.ListAll() {
			if entry.DefaultCost == nil || entry.Model == "" {
				continue
			}
			model := strings.ToLower(strings.TrimSpace(entry.Model))
			epType := strings.ToLower(strings.TrimSpace(entry.EndpointType))
			rates := usage.CostRates{
				InputPer1MTokens:      entry.DefaultCost.InputPer1MTokens,
				OutputPer1MTokens:     entry.DefaultCost.OutputPer1MTokens,
				CachedInputPer1MToken: entry.DefaultCost.CachedInputPer1MToken,
			}
			if epType != "" {
				table[epType+":"+model] = rates
			}
			table[model] = rates
		}
	}

	// Layer 2: Custom model_costs override (higher priority).
	for _, cost := range costs {
		model := strings.ToLower(strings.TrimSpace(cost.Model))
		epType := strings.ToLower(strings.TrimSpace(cost.EndpointType))
		if model == "" {
			continue
		}
		rates := usage.CostRates{
			InputPer1MTokens:      cost.InputPer1MTokens,
			OutputPer1MTokens:     cost.OutputPer1MTokens,
			CachedInputPer1MToken: cost.CachedInputPer1MToken,
		}
		if epType != "" {
			table[epType+":"+model] = rates
		}
		table[model] = rates
	}

	return table
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		// Last resort logging; cannot change response as headers already sent.
		// Use standard logger to avoid recursive dependency.
		slog.Default().Error("failed to encode admin response", "error", err)
	}
}

func errorResponse(message string) map[string]string {
	return map[string]string{"error": message}
}

// maskKey returns a masked version of a key for safe logging.
func maskKey(key string) string {
	if len(key) <= 8 {
		return "****"
	}
	return key[:4] + "..." + key[len(key)-4:]
}

// ===== Copilot Pool CRUD =====

func (h *Handler) handleListCopilotPools(w http.ResponseWriter, r *http.Request) {
	pools, err := h.copilotPoolStore.List()
	if err != nil {
		h.writeInternalError(w, "failed to list copilot pools", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pools": pools,
		"count": len(pools),
	})
}

func (h *Handler) handleCreateCopilotPool(w http.ResponseWriter, r *http.Request) {
	var req nosql.CopilotPool
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid json body"))
		return
	}

	// 校验关联的 targets 存在且均为 copilot 类型
	if err := h.validateCopilotTargets(req.Targets); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
		return
	}

	// 校验 client 存在
	if err := h.validateClientExists(req.ClientName); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
		return
	}

	if err := h.copilotPoolStore.Create(req); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "already exists") || strings.Contains(err.Error(), "already bound") {
			status = http.StatusConflict
		}
		writeJSON(w, status, errorResponse(err.Error()))
		return
	}

	// 同步 client.AllowedTargets
	if err := h.syncClientAllowedTargets(req.ClientName, req.Targets); err != nil {
		h.logger.Warn("copilot pool created but failed to sync client allowed_targets",
			"pool", req.Name, "client", req.ClientName, "error", err)
	}

	h.recordAudit(r, "copilot_pool_create", req.Name, "success", "client="+req.ClientName)
	writeJSON(w, http.StatusCreated, map[string]string{"status": "ok"})
}

func (h *Handler) handleUpdateCopilotPool(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if strings.TrimSpace(name) == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("missing pool name"))
		return
	}

	var req nosql.CopilotPool
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid json body"))
		return
	}

	// 获取旧 pool 信息（用于清理旧 client 的 targets）
	oldPool, err := h.copilotPoolStore.Get(name)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse(err.Error()))
		return
	}

	// 校验新 targets
	if err := h.validateCopilotTargets(req.Targets); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
		return
	}

	// 校验新 client 存在
	if err := h.validateClientExists(req.ClientName); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
		return
	}

	if err := h.copilotPoolStore.Update(name, req); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		} else if strings.Contains(err.Error(), "already exists") || strings.Contains(err.Error(), "already bound") {
			status = http.StatusConflict
		}
		writeJSON(w, status, errorResponse(err.Error()))
		return
	}

	// 如果 client 变更了，需要清理旧 client 的 targets
	if !strings.EqualFold(oldPool.ClientName, req.ClientName) {
		if err := h.removeTargetsFromClient(oldPool.ClientName, oldPool.Targets); err != nil {
			h.logger.Warn("failed to clean old client allowed_targets",
				"old_client", oldPool.ClientName, "error", err)
		}
	}

	// 同步新 client 的 AllowedTargets
	if err := h.syncClientAllowedTargets(req.ClientName, req.Targets); err != nil {
		h.logger.Warn("copilot pool updated but failed to sync client allowed_targets",
			"pool", req.Name, "client", req.ClientName, "error", err)
	}

	h.recordAudit(r, "copilot_pool_update", name, "success", "client="+req.ClientName)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) handleDeleteCopilotPool(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if strings.TrimSpace(name) == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("missing pool name"))
		return
	}

	// 获取 pool 信息用于清理 client targets
	pool, err := h.copilotPoolStore.Get(name)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse(err.Error()))
		return
	}

	if err := h.copilotPoolStore.Delete(name); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		writeJSON(w, status, errorResponse(err.Error()))
		return
	}

	// 从 client.AllowedTargets 中移除 pool 的 targets
	if err := h.removeTargetsFromClient(pool.ClientName, pool.Targets); err != nil {
		h.logger.Warn("copilot pool deleted but failed to clean client allowed_targets",
			"pool", name, "client", pool.ClientName, "error", err)
	}

	h.recordAudit(r, "copilot_pool_delete", name, "success", "")
	w.WriteHeader(http.StatusNoContent)
}

// validateCopilotTargets 校验所有 target 名称存在且 endpoint_type 为 copilot。
func (h *Handler) validateCopilotTargets(targets []string) error {
	if len(targets) == 0 {
		return nil // 空 targets 列表合法
	}
	cfg, err := h.currentConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	targetMap := make(map[string]config.Target, len(cfg.Targets))
	for _, t := range cfg.Targets {
		targetMap[strings.ToLower(t.Name)] = t
	}
	for _, tName := range targets {
		name := strings.ToLower(strings.TrimSpace(tName))
		t, ok := targetMap[name]
		if !ok {
			return fmt.Errorf("target %q not found", tName)
		}
		epType := config.NormalizeEndpointType(t.EndpointType)
		if epType != config.EndpointTypeCopilot {
			return fmt.Errorf("target %q is type %q, not copilot", tName, epType)
		}
	}
	return nil
}

// validateClientExists 校验 client 存在。
func (h *Handler) validateClientExists(clientName string) error {
	clientName = strings.TrimSpace(clientName)
	if clientName == "" {
		return errors.New("client_name must not be empty")
	}
	clients, err := h.clientStore.List()
	if err != nil {
		return fmt.Errorf("failed to list clients: %w", err)
	}
	for _, c := range clients {
		if strings.EqualFold(c.Name, clientName) {
			return nil
		}
	}
	return fmt.Errorf("client %q not found", clientName)
}

// syncClientAllowedTargets 将 targets 加入 client 的 AllowedTargets（去重），然后 reloadAuth。
func (h *Handler) syncClientAllowedTargets(clientName string, targets []string) error {
	clients, err := h.clientStore.List()
	if err != nil {
		return err
	}
	var found *config.Client
	for i := range clients {
		if strings.EqualFold(clients[i].Name, clientName) {
			found = &clients[i]
			break
		}
	}
	if found == nil {
		return fmt.Errorf("client %q not found", clientName)
	}

	// 构建已有 set
	existing := make(map[string]struct{}, len(found.AllowedTargets))
	for _, t := range found.AllowedTargets {
		existing[strings.ToLower(t)] = struct{}{}
	}

	changed := false
	for _, t := range targets {
		key := strings.ToLower(strings.TrimSpace(t))
		if key == "" {
			continue
		}
		if _, ok := existing[key]; !ok {
			found.AllowedTargets = append(found.AllowedTargets, key)
			existing[key] = struct{}{}
			changed = true
		}
	}

	if !changed {
		return nil
	}

	if err := h.clientStore.Update(found.Name, *found); err != nil {
		return err
	}
	return h.reloadAuthFromClientStore()
}

// removeTargetsFromClient 从 client 的 AllowedTargets 中移除指定 targets，然后 reloadAuth。
func (h *Handler) removeTargetsFromClient(clientName string, targets []string) error {
	if len(targets) == 0 {
		return nil
	}
	clients, err := h.clientStore.List()
	if err != nil {
		return err
	}
	var found *config.Client
	for i := range clients {
		if strings.EqualFold(clients[i].Name, clientName) {
			found = &clients[i]
			break
		}
	}
	if found == nil {
		return nil // client 已不存在，无需清理
	}

	// 如果 AllowedTargets 为空（allowAll），不做操作
	if len(found.AllowedTargets) == 0 {
		return nil
	}

	removeSet := make(map[string]struct{}, len(targets))
	for _, t := range targets {
		removeSet[strings.ToLower(strings.TrimSpace(t))] = struct{}{}
	}

	newTargets := make([]string, 0, len(found.AllowedTargets))
	for _, t := range found.AllowedTargets {
		if _, remove := removeSet[strings.ToLower(t)]; !remove {
			newTargets = append(newTargets, t)
		}
	}

	if len(newTargets) == len(found.AllowedTargets) {
		return nil // nothing changed
	}

	found.AllowedTargets = newTargets
	if err := h.clientStore.Update(found.Name, *found); err != nil {
		return err
	}
	return h.reloadAuthFromClientStore()
}

// ===== Copilot Account Management =====

// copilotAccountResponse 用于 API 响应，脱敏 token 字段。
type copilotAccountResponse struct {
	nosql.CopilotAccount
	HasOAuthToken   bool `json:"has_oauth_token"`
	HasCopilotToken bool `json:"has_copilot_token"`
}

// sanitizeCopilotAccount 脱敏处理，隐藏 token 原文。
func sanitizeCopilotAccount(a nosql.CopilotAccount) copilotAccountResponse {
	resp := copilotAccountResponse{
		CopilotAccount:  a,
		HasOAuthToken:   a.OAuthToken != "",
		HasCopilotToken: a.CopilotToken != "",
	}
	resp.OAuthToken = ""
	resp.CopilotToken = ""
	resp.DeviceCode = ""
	return resp
}

// requireCopilotService 检查 copilot 服务是否已配置，未配置时返回 503。
func (h *Handler) requireCopilotService(w http.ResponseWriter) bool {
	if h.copilotService == nil || h.copilotAcctStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse("copilot service not configured"))
		return false
	}
	return true
}

func (h *Handler) handleListCopilotAccounts(w http.ResponseWriter, r *http.Request) {
	if !h.requireCopilotService(w) {
		return
	}

	poolName := strings.TrimSpace(r.URL.Query().Get("pool_name"))
	var accounts []nosql.CopilotAccount
	var err error
	if poolName != "" {
		accounts, err = h.copilotAcctStore.ListByPool(poolName)
	} else {
		accounts, err = h.copilotAcctStore.List()
	}
	if err != nil {
		h.writeInternalError(w, "failed to list copilot accounts", err)
		return
	}

	resp := make([]copilotAccountResponse, len(accounts))
	for i, a := range accounts {
		resp[i] = sanitizeCopilotAccount(a)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": resp,
		"count":    len(resp),
	})
}

func (h *Handler) handleGetCopilotAccount(w http.ResponseWriter, r *http.Request) {
	if !h.requireCopilotService(w) {
		return
	}

	id := strings.TrimSpace(chi.URLParam(r, "id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("missing account id"))
		return
	}

	account, err := h.copilotAcctStore.Get(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse(err.Error()))
		return
	}

	writeJSON(w, http.StatusOK, sanitizeCopilotAccount(*account))
}

func (h *Handler) handleStartCopilotAuth(w http.ResponseWriter, r *http.Request) {
	if !h.requireCopilotService(w) {
		return
	}

	var req struct {
		PoolName string `json:"pool_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid json body"))
		return
	}
	poolName := strings.TrimSpace(req.PoolName)
	if poolName == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("pool_name must not be empty"))
		return
	}

	accountID, userCode, verificationURI, err := h.copilotService.InitiateAuth(r.Context(), poolName)
	if err != nil {
		h.writeInternalError(w, "failed to initiate copilot auth", err)
		return
	}

	h.recordAudit(r, "copilot.auth.start", accountID, "success", fmt.Sprintf("pool=%s", poolName))
	writeJSON(w, http.StatusOK, map[string]any{
		"account_id":       accountID,
		"user_code":        userCode,
		"verification_uri": verificationURI,
		"message":          "请在浏览器中访问 verification_uri 并输入 user_code 完成授权",
	})
}

func (h *Handler) handleCompleteCopilotAuth(w http.ResponseWriter, r *http.Request) {
	if !h.requireCopilotService(w) {
		return
	}

	id := strings.TrimSpace(chi.URLParam(r, "id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("missing account id"))
		return
	}

	// CompleteAuth 会阻塞等待用户授权（PollForToken），设置 10 分钟超时
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()

	if err := h.copilotService.CompleteAuth(ctx, id); err != nil {
		h.writeInternalError(w, "failed to complete copilot auth", err)
		return
	}

	h.recordAudit(r, "copilot.auth.complete", id, "success", "")
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) handleRevokeCopilotAccount(w http.ResponseWriter, r *http.Request) {
	if !h.requireCopilotService(w) {
		return
	}

	id := strings.TrimSpace(chi.URLParam(r, "id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("missing account id"))
		return
	}

	if err := h.copilotService.RevokeAuth(r.Context(), id); err != nil {
		h.writeInternalError(w, "failed to revoke copilot account", err)
		return
	}

	h.recordAudit(r, "copilot.auth.revoke", id, "success", "")
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) handleDisableCopilotAccount(w http.ResponseWriter, r *http.Request) {
	if !h.requireCopilotService(w) {
		return
	}

	id := strings.TrimSpace(chi.URLParam(r, "id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("missing account id"))
		return
	}

	account, err := h.copilotAcctStore.Get(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse(err.Error()))
		return
	}

	account.Status = nosql.AccountStatusDisabled
	if err := h.copilotAcctStore.Update(id, *account); err != nil {
		h.writeInternalError(w, "failed to disable copilot account", err)
		return
	}

	h.recordAudit(r, "copilot.account.disable", id, "success", "")
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) handleEnableCopilotAccount(w http.ResponseWriter, r *http.Request) {
	if !h.requireCopilotService(w) {
		return
	}

	id := strings.TrimSpace(chi.URLParam(r, "id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("missing account id"))
		return
	}

	account, err := h.copilotAcctStore.Get(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse(err.Error()))
		return
	}

	account.Status = nosql.AccountStatusActive
	if err := h.copilotAcctStore.Update(id, *account); err != nil {
		h.writeInternalError(w, "failed to enable copilot account", err)
		return
	}

	h.recordAudit(r, "copilot.account.enable", id, "success", "")
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) handleDeleteCopilotAccount(w http.ResponseWriter, r *http.Request) {
	if !h.requireCopilotService(w) {
		return
	}

	id := strings.TrimSpace(chi.URLParam(r, "id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("missing account id"))
		return
	}

	if err := h.copilotAcctStore.Delete(id); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		writeJSON(w, status, errorResponse(err.Error()))
		return
	}

	h.recordAudit(r, "copilot.account.delete", id, "success", "")
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleGetCopilotQuota(w http.ResponseWriter, r *http.Request) {
	if !h.requireCopilotService(w) {
		return
	}

	id := strings.TrimSpace(chi.URLParam(r, "id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("missing account id"))
		return
	}

	account, err := h.copilotAcctStore.Get(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse(err.Error()))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"account_id":              id,
		"quota_percent_remaining": account.QuotaPercentRemaining,
		"quota_reset_at":          account.QuotaResetAt,
		"quota_last_sync_at":      account.QuotaLastSyncAt,
	})
}

func (h *Handler) handleSyncCopilotQuota(w http.ResponseWriter, r *http.Request) {
	if !h.requireCopilotService(w) {
		return
	}
	if h.copilotQuotaMgr == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse("copilot quota manager not configured"))
		return
	}

	id := strings.TrimSpace(chi.URLParam(r, "id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("missing account id"))
		return
	}

	account, err := h.copilotAcctStore.Get(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse(err.Error()))
		return
	}

	if account.OAuthToken == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("account has no oauth token"))
		return
	}

	quotaInfo, err := h.copilotQuotaMgr.SyncQuotaFromGitHub(r.Context(), account.OAuthToken)
	if err != nil {
		h.writeInternalError(w, "failed to sync copilot quota", err)
		return
	}

	// 更新账户额度字段
	account.QuotaPercentRemaining = quotaInfo.PercentRemaining
	if quotaInfo.ResetAt != "" {
		account.QuotaResetAt = quotaInfo.ResetAt
	}
	account.QuotaLastSyncAt = time.Now().UTC().Format(time.RFC3339)

	if err := h.copilotAcctStore.Update(id, *account); err != nil {
		h.writeInternalError(w, "failed to update copilot account quota", err)
		return
	}

	h.recordAudit(r, "copilot.quota.sync", id, "success", fmt.Sprintf("remaining=%.1f%%", quotaInfo.PercentRemaining))
	writeJSON(w, http.StatusOK, map[string]any{
		"account_id":              id,
		"quota_percent_remaining": quotaInfo.PercentRemaining,
		"quota_reset_at":          quotaInfo.ResetAt,
		"quota_last_sync_at":      account.QuotaLastSyncAt,
		"copilot_plan":            quotaInfo.CopilotPlan,
	})
}

func (h *Handler) handleListCopilotModels(w http.ResponseWriter, r *http.Request) {
	// 尝试从 Copilot API 动态获取可用模型（需要至少一个 active 账户的 token）
	if h.copilotService != nil && h.copilotAcctStore != nil {
		accounts, err := h.copilotAcctStore.List()
		if err == nil {
			for _, acct := range accounts {
				if acct.Status != nosql.AccountStatusActive || acct.OAuthToken == "" {
					continue
				}
				// 获取 copilot access token（也会刷新并保存 APIBaseURL）
				token, err := h.copilotService.GetToken(r.Context(), acct.ID)
				if err != nil {
					continue
				}
				// 重新读取账户以获取更新后的 APIBaseURL
				refreshedAcct, err := h.copilotAcctStore.Get(acct.ID)
				if err != nil {
					refreshedAcct = &acct
				}
				// 动态构造 models URL
				modelsURL := copilot.CopilotModelsURL // 默认 individual
				if refreshedAcct.APIBaseURL != "" {
					modelsURL = strings.TrimRight(refreshedAcct.APIBaseURL, "/") + "/models"
				}
				models, err := copilot.FetchModelsFromAPI(r.Context(), nil, token, modelsURL)
				if err != nil {
					h.logger.Warn("从 Copilot API 获取模型列表失败，降级为本地列表",
						"account_id", acct.ID, "models_url", modelsURL, "error", err)
					continue
				}
				writeJSON(w, http.StatusOK, map[string]any{
					"models": models,
					"count":  len(models),
					"source": "copilot_api",
				})
				return
			}
		}
	}

	// 降级：返回本地硬编码乘数表
	models := copilot.ListAvailableModels()
	writeJSON(w, http.StatusOK, map[string]any{
		"models": models,
		"count":  len(models),
		"source": "local",
	})
}
