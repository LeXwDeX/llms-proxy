package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
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
	targetStore      *nosql.TargetStore
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
	targetStore *nosql.TargetStore,
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
		targetStore:      targetStore,
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
		r.Put("/targets/{name}/pause", h.handlePauseTarget)
		r.Put("/targets/{name}/resume", h.handleResumeTarget)
		r.Put("/targets/{name}/keys/{index}/block", h.handleBlockTargetKey)
		r.Put("/targets/{name}/keys/{index}/unblock", h.handleUnblockTargetKey)
		r.Post("/targets/{name}/keys/wakeup", h.handleWakeUpTargetKeys)

		// Copilot pool management
		r.Get("/copilot-pools", h.handleListCopilotPools)
		r.Post("/copilot-pools", h.handleCreateCopilotPool)
		r.Put("/copilot-pools/{name}", h.handleUpdateCopilotPool)
		r.Delete("/copilot-pools/{name}", h.handleDeleteCopilotPool)
		r.Get("/copilot-pools/{name}/models", h.handleListCopilotPoolModels)

		// Copilot account management
		r.Get("/copilot-accounts", h.handleListCopilotAccounts)
		r.Get("/copilot-accounts/{id}", h.handleGetCopilotAccount)
		r.Post("/copilot-accounts/auth/start", h.handleStartCopilotAuth)
		r.Post("/copilot-accounts/auth/complete/{id}", h.handleCompleteCopilotAuth)
		r.Post("/copilot-accounts/{id}/revoke", h.handleRevokeCopilotAccount)
		r.Post("/copilot-accounts/{id}/disable", h.handleDisableCopilotAccount)
		r.Post("/copilot-accounts/{id}/enable", h.handleEnableCopilotAccount)
		r.Post("/copilot-accounts/{id}/toggle-overage", h.handleToggleCopilotOverage)
		r.Post("/copilot-accounts/{id}/refresh-token", h.handleRefreshCopilotToken)
		r.Delete("/copilot-accounts/{id}", h.handleDeleteCopilotAccount)
		r.Get("/copilot-accounts/{id}/quota", h.handleGetCopilotQuota)
		r.Post("/copilot-accounts/{id}/quota/sync", h.handleSyncCopilotQuota)
		r.Get("/copilot-models", h.handleListCopilotModels)

		// Model catalog
		r.Get("/catalog", h.handleListCatalog)
		r.Get("/catalog/{endpoint_type}", h.handleListCatalogByType)

		// Endpoint type metadata（单一信息源，UI 下拉/徽章数据）
		r.Get("/endpoint-types", h.handleListEndpointTypes)

		// Trace store (DEBUG mode)
		r.Get("/trace", h.handleListTrace)
		r.Get("/trace/{request_id}", h.handleGetTrace)
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
			Total         int64 `json:"total"`
			Success       int64 `json:"success"`
			Failures      int64 `json:"failures"`
			Retries       int64 `json:"retries"`
			KeyRetries    int64 `json:"key_retries"`
			TargetRetries int64 `json:"target_retries"`
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
	response.Requests.KeyRetries = metrics.TotalKeyRetries
	response.Requests.TargetRetries = metrics.TotalTargetRetries

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
		if !t.Muted && !t.Paused {
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
			"total":          metrics.TotalRequests,
			"success":        metrics.TotalSuccess,
			"failures":       metrics.TotalFailures,
			"retries":        metrics.TotalRetries,
			"key_retries":    metrics.TotalKeyRetries,
			"target_retries": metrics.TotalTargetRetries,
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
	if err := h.overlayStoredTargets(cfg); err != nil {
		h.writeInternalError(w, "failed to load targets", err)
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

	if h.targetStore != nil && len(cfg.Targets) > 0 {
		if _, err := h.targetStore.MigrateFromConfig(cfg.Targets); err != nil {
			h.writeInternalError(w, "failed to persist targets", err)
			return
		}
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

	// Copilot 模型走订阅额度，不支持 token 计费
	if strings.HasPrefix(model, copilot.ModelPrefix) || strings.HasPrefix(strings.ToLower(model), strings.ToLower(copilot.ModelPrefix)) {
		writeJSON(w, http.StatusBadRequest, errorResponse("copilot models use subscription quota, not token billing"))
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

func targetNameParam(r *http.Request) (string, error) {
	name, err := url.PathUnescape(chi.URLParam(r, "name"))
	if err != nil {
		return "", errors.New("invalid target name")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("name must not be empty")
	}
	return name, nil
}

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

		// Mask API keys for security (only return masked versions)
		maskedAPIKey := ""
		if t.APIKey != "" {
			maskedAPIKey = maskKey(t.APIKey)
		}
		maskedAPIKeys := make([]string, len(t.APIKeys))
		for j, k := range t.APIKeys {
			maskedAPIKeys[j] = maskKey(k)
		}

		m := map[string]any{
			"name":                     t.Name,
			"endpoint_type":            epType,
			"endpoint":                 t.Endpoint,
			"resource_path_prefix":     t.ResourcePathPrefix,
			"has_api_key":              t.APIKey != "",
			"api_key":                  maskedAPIKey,
			"api_keys":                 maskedAPIKeys,
			"api_key_count":            len(t.APIKeys),
			"key_reset_time":           t.KeyResetTime,
			"provider_class":           t.ProviderClass,
			"paused":                   t.Paused,
			"allow_bearer_passthrough": t.AllowBearer,
			"auth_mode":                t.AuthMode,
			"allowed_models":           t.AllowedModels,
			"sse_auto_aggregate":       sseAutoAgg,
			"openai_prefix":            t.OpenAIPrefix,
			"anthropic_prefix":         t.AnthropicPrefix,
			"supports_responses":       t.SupportsResponses,
		}
		// 附加 key 池运行时状态
		if statuses := h.proxyService.KeyPoolStatus(t.Name); statuses != nil {
			m["key_statuses"] = statuses
		}
		targets[i] = m
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
		APIKeys            []string `json:"api_keys"`
		KeyResetTime       string   `json:"key_reset_time"`
		Paused             bool     `json:"paused"`
		AllowBearer        bool     `json:"allow_bearer_passthrough"`
		AuthMode           string   `json:"auth_mode"`
		AllowedModels      []string `json:"allowed_models"`
		SSEAutoAggregate   *bool    `json:"sse_auto_aggregate,omitempty"`
		OpenAIPrefix       string   `json:"openai_prefix"`
		AnthropicPrefix    string   `json:"anthropic_prefix"`
		SupportsResponses  bool     `json:"supports_responses"`
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
		APIKeys:            body.APIKeys,
		KeyResetTime:       body.KeyResetTime,
		Paused:             body.Paused,
		AllowBearer:        body.AllowBearer,
		AuthMode:           body.AuthMode,
		AllowedModels:      body.AllowedModels,
		SSEAutoAggregate:   body.SSEAutoAggregate,
		OpenAIPrefix:       body.OpenAIPrefix,
		AnthropicPrefix:    body.AnthropicPrefix,
		SupportsResponses:  body.SupportsResponses,
	}
	cfg.Targets = append(cfg.Targets, newTarget)

	if err := h.applyConfigRuntime(cfg); err != nil {
		h.writeInternalError(w, "failed to apply config", err)
		return
	}

	if err := h.targetStore.Create(newTarget); err != nil {
		h.writeInternalError(w, "failed to persist target", err)
		return
	}
	h.configManager.Replace(cfg)

	h.recordAudit(r, "create_target", name, "success", "")
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "name": name})
}

func (h *Handler) handleUpdateTarget(w http.ResponseWriter, r *http.Request) {
	name, err := targetNameParam(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
		return
	}

	var body struct {
		EndpointType       string    `json:"endpoint_type"`
		Endpoint           string    `json:"endpoint"`
		ResourcePathPrefix string    `json:"resource_path_prefix"`
		APIKey             *string   `json:"api_key"`
		APIKeys            *[]string `json:"api_keys"`
		KeyResetTime       *string   `json:"key_reset_time"`
		Paused             *bool     `json:"paused"`
		AllowBearer        bool      `json:"allow_bearer_passthrough"`
		AuthMode           *string   `json:"auth_mode"`
		AllowedModels      []string  `json:"allowed_models"`
		SSEAutoAggregate   *bool     `json:"sse_auto_aggregate,omitempty"`
		OpenAIPrefix       *string   `json:"openai_prefix"`
		AnthropicPrefix    *string   `json:"anthropic_prefix"`
		SupportsResponses  *bool     `json:"supports_responses"`
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
			existingKeys := existingTargetKeys(*t)
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
				resolved, err := resolveSubmittedTargetKey(*body.APIKey, existingKeys)
				if err != nil {
					writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
					return
				}
				t.APIKey = resolved
			}
			t.AllowBearer = body.AllowBearer
			t.AllowedModels = body.AllowedModels
			if body.AuthMode != nil {
				t.AuthMode = strings.TrimSpace(*body.AuthMode)
			}
			if body.SSEAutoAggregate != nil {
				t.SSEAutoAggregate = body.SSEAutoAggregate
			}
			if body.APIKeys != nil {
				resolvedKeys := make([]string, 0, len(*body.APIKeys))
				for _, k := range *body.APIKeys {
					resolved, err := resolveSubmittedTargetKey(k, existingKeys)
					if err != nil {
						writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
						return
					}
					if resolved != "" {
						resolvedKeys = append(resolvedKeys, resolved)
					}
				}
				t.APIKeys = resolvedKeys
			}
			if body.KeyResetTime != nil {
				t.KeyResetTime = *body.KeyResetTime
			}
			if body.Paused != nil {
				t.Paused = *body.Paused
			}
			if body.OpenAIPrefix != nil {
				t.OpenAIPrefix = *body.OpenAIPrefix
			}
			if body.AnthropicPrefix != nil {
				t.AnthropicPrefix = *body.AnthropicPrefix
			}
			if body.SupportsResponses != nil {
				t.SupportsResponses = *body.SupportsResponses
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

	var updatedTarget config.Target
	for _, target := range cfg.Targets {
		if strings.EqualFold(target.Name, name) {
			updatedTarget = target
			break
		}
	}
	if err := h.targetStore.Update(name, updatedTarget); err != nil {
		h.writeInternalError(w, "failed to persist target", err)
		return
	}
	h.configManager.Replace(cfg)

	h.recordAudit(r, "update_target", name, "success", "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": name})
}

const existingTargetKeyRefPrefix = "__existing_key_index__:"

func existingTargetKeys(t config.Target) []string {
	keys := make([]string, 0, 1+len(t.APIKeys))
	keys = append(keys, t.APIKey)
	keys = append(keys, t.APIKeys...)
	return keys
}

func resolveSubmittedTargetKey(submitted string, existing []string) (string, error) {
	trimmed := strings.TrimSpace(submitted)
	if trimmed == "" {
		return "", nil
	}
	if strings.HasPrefix(trimmed, existingTargetKeyRefPrefix) {
		rawIndex := strings.TrimPrefix(trimmed, existingTargetKeyRefPrefix)
		idx, err := strconv.Atoi(rawIndex)
		if err != nil || idx < 0 || idx >= len(existing) || existing[idx] == "" {
			return "", fmt.Errorf("invalid existing key reference")
		}
		return existing[idx], nil
	}

	matched := ""
	for _, key := range existing {
		if key != "" && trimmed == maskKey(key) {
			if matched != "" {
				return "", fmt.Errorf("ambiguous masked api key")
			}
			matched = key
		}
	}
	if matched != "" {
		return matched, nil
	}
	if looksLikeMaskedTargetKey(trimmed) {
		return "", fmt.Errorf("unrecognized masked api key")
	}
	return trimmed, nil
}

func looksLikeMaskedTargetKey(value string) bool {
	return value == "****" || strings.Contains(value, "...")
}

func (h *Handler) handleDeleteTarget(w http.ResponseWriter, r *http.Request) {
	name, err := targetNameParam(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
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

	if err := h.targetStore.Delete(name); err != nil {
		h.writeInternalError(w, "failed to persist target deletion", err)
		return
	}
	h.configManager.Replace(cfg)

	h.recordAudit(r, "delete_target", name, "success", "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) handlePauseTarget(w http.ResponseWriter, r *http.Request) {
	h.handleSetTargetPaused(w, r, true)
}

func (h *Handler) handleResumeTarget(w http.ResponseWriter, r *http.Request) {
	h.handleSetTargetPaused(w, r, false)
}

func (h *Handler) handleSetTargetPaused(w http.ResponseWriter, r *http.Request, paused bool) {
	name, err := targetNameParam(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
		return
	}

	cfg, err := h.currentConfig()
	if err != nil {
		h.writeInternalError(w, "failed to load config", err)
		return
	}

	found := false
	var updatedTarget config.Target
	for i := range cfg.Targets {
		if strings.EqualFold(cfg.Targets[i].Name, name) {
			cfg.Targets[i].Paused = paused
			updatedTarget = cfg.Targets[i]
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
	if err := h.targetStore.Update(name, updatedTarget); err != nil {
		h.writeInternalError(w, "failed to persist target", err)
		return
	}
	h.configManager.Replace(cfg)

	action := "resume_target"
	if paused {
		action = "pause_target"
	}
	h.recordAudit(r, action, name, "success", "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": updatedTarget.Name, "paused": paused})
}

func (h *Handler) handleBlockTargetKey(w http.ResponseWriter, r *http.Request) {
	name, nameErr := targetNameParam(r)
	indexStr := chi.URLParam(r, "index")
	if nameErr != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(nameErr.Error()))
		return
	}
	index, err := strconv.Atoi(indexStr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid key index"))
		return
	}

	if err := h.proxyService.BlockKey(name, index); err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse(err.Error()))
		return
	}

	h.recordAudit(r, "block_target_key", name, "success", fmt.Sprintf("key_index=%d", index))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) handleUnblockTargetKey(w http.ResponseWriter, r *http.Request) {
	name, nameErr := targetNameParam(r)
	indexStr := chi.URLParam(r, "index")
	if nameErr != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(nameErr.Error()))
		return
	}
	index, err := strconv.Atoi(indexStr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid key index"))
		return
	}

	if err := h.proxyService.UnblockKey(name, index); err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse(err.Error()))
		return
	}

	h.recordAudit(r, "unblock_target_key", name, "success", fmt.Sprintf("key_index=%d", index))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) handleWakeUpTargetKeys(w http.ResponseWriter, r *http.Request) {
	name, err := targetNameParam(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
		return
	}

	recovered, err := h.proxyService.WakeUpKeys(name)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse(err.Error()))
		return
	}

	h.recordAudit(r, "wakeup_target_keys", name, "success", fmt.Sprintf("recovered=%d", recovered))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "recovered": recovered})
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

// handleListEndpointTypes 暴露 endpoint_type 全集元数据，供 admin UI 渲染下拉、徽章。
//
// 单一信息源约束：UI 不得自己维护硬编码列表/标签/配色，必须从此 API 拉取。
// 数据源在 internal/config/endpoint_type.go 的 endpointTypes。
func (h *Handler) handleListEndpointTypes(w http.ResponseWriter, r *http.Request) {
	metas := config.AllEndpointTypeMetas()
	writeJSON(w, http.StatusOK, map[string]any{
		"endpoint_types": metas,
		"count":          len(metas),
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
	if err := h.applyStoredTargets(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (h *Handler) applyStoredTargets(cfg *config.Config) error {
	if h.targetStore == nil {
		return nil
	}
	targets, err := h.targetStore.List()
	if err != nil {
		return err
	}
	if len(targets) == 0 && len(cfg.Targets) > 0 {
		if _, err := h.targetStore.MigrateFromConfig(cfg.Targets); err != nil {
			return err
		}
		targets, err = h.targetStore.List()
		if err != nil {
			return err
		}
	}
	if len(targets) > 0 || len(cfg.Targets) == 0 {
		cfg.Targets = targets
	}
	return nil
}

func (h *Handler) overlayStoredTargets(cfg *config.Config) error {
	if h.targetStore == nil {
		return nil
	}
	targets, err := h.targetStore.List()
	if err != nil {
		return err
	}
	if len(targets) > 0 || len(cfg.Targets) == 0 {
		cfg.Targets = targets
	}
	return nil
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

// handleGetTrace 按 request_id 查询单条 trace 记录。
// 仅供 Admin UI 内部使用，不对外暴露 API。
func (h *Handler) handleGetTrace(w http.ResponseWriter, r *http.Request) {
	// 检查请求来源，确保来自 Admin UI
	if !isAdminUIRequest(r) {
		writeJSON(w, http.StatusForbidden, errorResponse("access denied"))
		return
	}

	requestID := chi.URLParam(r, "request_id")
	if requestID == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("request_id required"))
		return
	}

	record := h.proxyService.GetTrace(requestID)
	if record == nil {
		writeJSON(w, http.StatusNotFound, errorResponse("trace not found"))
		return
	}

	writeJSON(w, http.StatusOK, record)
}

// handleListTrace 列出最近的 trace 记录。
// 仅供 Admin UI 内部使用，不对外暴露 API。
func (h *Handler) handleListTrace(w http.ResponseWriter, r *http.Request) {
	// 检查请求来源，确保来自 Admin UI
	if !isAdminUIRequest(r) {
		writeJSON(w, http.StatusForbidden, errorResponse("access denied"))
		return
	}

	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}

	records := h.proxyService.ListTrace(limit)
	stats := h.proxyService.TraceStats()

	writeJSON(w, http.StatusOK, map[string]any{
		"records": records,
		"count":   len(records),
		"stats":   stats,
	})
}

// isAdminUIRequest 检查请求是否来自 Admin UI。
// 通过检查 X-Requested-With 头来判断（UI 的 api() 函数会发送此头）。
func isAdminUIRequest(r *http.Request) bool {
	return r.Header.Get("X-Requested-With") == "XMLHttpRequest"
}
