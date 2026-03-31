package admin

import (
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
	"github.com/ycgame/llms-proxy/internal/nosql"
	"github.com/ycgame/llms-proxy/internal/proxy"
	"github.com/ycgame/llms-proxy/internal/usage"
)

// Handler wires administration endpoints.
type Handler struct {
	configManager *config.Manager
	authStore     *auth.Store
	proxyService  *proxy.Service
	auditStore    *AuditStore
	userStore     *UserStore
	modelCatalog  *catalog.Catalog
	logger        *slog.Logger
}

// NewHandler constructs an HTTP router exposing admin endpoints.
func NewHandler(
	manager *config.Manager,
	store *auth.Store,
	service *proxy.Service,
	auditStore *AuditStore,
	userStore *UserStore,
	modelCatalog *catalog.Catalog,
	logger *slog.Logger,
) http.Handler {
	h := &Handler{
		configManager: manager,
		authStore:     store,
		proxyService:  service,
		auditStore:    auditStore,
		userStore:     userStore,
		modelCatalog:  modelCatalog,
		logger:        logger,
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
	if _, err := h.userStore.Authenticate(session.Username, req.OldPassword); err != nil {
		writeJSON(w, http.StatusForbidden, errorResponse("原密码验证失败"))
		return
	}

	// Generate new hash with random salt.
	newHash, err := HashPasswordWithRandomSalt(req.NewPassword)
	if err != nil {
		h.writeInternalError(w, "failed to hash password", err)
		return
	}

	if err := h.userStore.UpdatePasswordHash(session.Username, newHash); err != nil {
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

	clientStore, err := h.currentClientStore()
	if err != nil {
		h.writeInternalError(w, "failed to load clients store", err)
		return
	}
	clients, err := clientStore.List()
	if err != nil {
		h.writeInternalError(w, "failed to list clients", err)
		return
	}
	modelStore, err := h.currentModelCostStore()
	if err != nil {
		h.writeInternalError(w, "failed to load model costs store", err)
		return
	}
	models, err := modelStore.List()
	if err != nil {
		h.writeInternalError(w, "failed to list model costs", err)
		return
	}
	usageStore, err := h.currentUsageStore()
	if err != nil {
		h.writeInternalError(w, "failed to load usage store", err)
		return
	}
	costs, err := modelStore.List()
	if err != nil {
		h.writeInternalError(w, "failed to list model costs", err)
		return
	}
	summary, err := usageStore.Summary(now, toUsageCostTable(costs, h.modelCatalog))
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
	events72h, _ := usageStore.List(usage.Filter{From: &from72h, To: &now, Limit: 0})
	var reqs72h, success72h int64
	for _, evt := range events72h {
		reqs72h++
		if evt.StatusCode > 0 && evt.StatusCode < 500 {
			success72h++
		}
	}
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
		"model_cost_count": len(models),
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

	tempStore := auth.NewStore()
	clientStore := nosql.NewClientStore(cfg.DataFiles.ClientsFile)
	clients, err := clientStore.List()
	if err != nil {
		h.logger.Warn("admin reload rejected: invalid clients file",
			"error", err,
		)
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid clients file"))
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
	h.recordAudit(r, "config_reload", path, "success", fmt.Sprintf("targets=%d clients=%d", len(cfg.AzureTargets), len(clients)))
	h.logger.Info("configuration reloaded via admin endpoint",
		"path", path,
		"targets", len(cfg.AzureTargets),
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
		Targets:    len(cfg.AzureTargets),
		Clients:    len(clients),
	}

	writeJSON(w, http.StatusOK, response)
}

func (h *Handler) handleListClients(w http.ResponseWriter, r *http.Request) {
	store, err := h.currentClientStore()
	if err != nil {
		h.writeInternalError(w, "failed to load clients store", err)
		return
	}

	clients, err := store.List()
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
	store, err := h.currentClientStore()
	if err != nil {
		h.writeInternalError(w, "failed to load clients store", err)
		return
	}

	var req config.Client
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid json body"))
		return
	}

	if err := store.Create(req); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "already exists") {
			status = http.StatusConflict
		}
		writeJSON(w, status, errorResponse(err.Error()))
		return
	}

	if err := h.reloadAuthFromClientStore(store); err != nil {
		h.writeInternalError(w, "failed to apply auth store", err)
		return
	}
	h.recordAudit(r, "client_create", req.Name, "success", maskKey(req.AccessKey))

	writeJSON(w, http.StatusCreated, map[string]string{"status": "ok"})
}

func (h *Handler) handleUpdateClient(w http.ResponseWriter, r *http.Request) {
	store, err := h.currentClientStore()
	if err != nil {
		h.writeInternalError(w, "failed to load clients store", err)
		return
	}

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

	if err := store.Update(name, req); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		} else if strings.Contains(err.Error(), "already exists") {
			status = http.StatusConflict
		}
		writeJSON(w, status, errorResponse(err.Error()))
		return
	}

	if err := h.reloadAuthFromClientStore(store); err != nil {
		h.writeInternalError(w, "failed to apply auth store", err)
		return
	}
	h.recordAudit(r, "client_update", name, "success", maskKey(req.AccessKey))

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) handleDeleteClient(w http.ResponseWriter, r *http.Request) {
	store, err := h.currentClientStore()
	if err != nil {
		h.writeInternalError(w, "failed to load clients store", err)
		return
	}

	name := chi.URLParam(r, "name")
	if strings.TrimSpace(name) == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("missing client name"))
		return
	}

	if err := store.Delete(name); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		writeJSON(w, status, errorResponse(err.Error()))
		return
	}

	if err := h.reloadAuthFromClientStore(store); err != nil {
		h.writeInternalError(w, "failed to apply auth store", err)
		return
	}
	h.recordAudit(r, "client_delete", name, "success", "")

	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleListModelCosts(w http.ResponseWriter, r *http.Request) {
	store, err := h.currentModelCostStore()
	if err != nil {
		h.writeInternalError(w, "failed to load model costs store", err)
		return
	}

	models, err := store.List()
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
	store, err := h.currentModelCostStore()
	if err != nil {
		h.writeInternalError(w, "failed to load model costs store", err)
		return
	}

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
		_ = store.DeleteByKey(originalEpType, model)
	}

	cost := nosql.ModelCost{
		EndpointType:          req.EndpointType,
		Model:                 model,
		InputPer1MTokens:      req.InputPer1MTokens,
		OutputPer1MTokens:     req.OutputPer1MTokens,
		CachedInputPer1MToken: req.CachedInputPer1MToken,
	}
	if err := store.Upsert(cost); err != nil {
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
	store, err := h.currentModelCostStore()
	if err != nil {
		h.writeInternalError(w, "failed to load model costs store", err)
		return
	}

	model := strings.TrimSpace(chi.URLParam(r, "model"))
	if model == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("missing model"))
		return
	}

	endpointType := r.URL.Query().Get("endpoint_type")
	var deleteErr error
	if endpointType != "" {
		deleteErr = store.DeleteByKey(endpointType, model)
	} else {
		deleteErr = store.Delete(model)
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
	store, err := h.currentUsageStore()
	if err != nil {
		h.writeInternalError(w, "failed to load usage store", err)
		return
	}

	filter, err := parseUsageFilter(r, true)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
		return
	}

	events, err := store.List(filter)
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
	usageStore, err := h.currentUsageStore()
	if err != nil {
		h.writeInternalError(w, "failed to load usage store", err)
		return
	}
	modelCostStore, err := h.currentModelCostStore()
	if err != nil {
		h.writeInternalError(w, "failed to load model costs store", err)
		return
	}

	filter, err := parseUsageFilter(r, false)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
		return
	}
	costs, err := modelCostStore.List()
	if err != nil {
		h.writeInternalError(w, "failed to list model costs", err)
		return
	}

	result, err := usageStore.Aggregate(filter, r.URL.Query().Get("group_by"), toUsageCostTable(costs, h.modelCatalog))
	if err != nil {
		h.writeInternalError(w, "failed to aggregate usage", err)
		return
	}

	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) handleUsageSummary(w http.ResponseWriter, r *http.Request) {
	usageStore, err := h.currentUsageStore()
	if err != nil {
		h.writeInternalError(w, "failed to load usage store", err)
		return
	}
	modelCostStore, err := h.currentModelCostStore()
	if err != nil {
		h.writeInternalError(w, "failed to load model costs store", err)
		return
	}

	costs, err := modelCostStore.List()
	if err != nil {
		h.writeInternalError(w, "failed to list model costs", err)
		return
	}

	result, err := usageStore.Summary(time.Now().UTC(), toUsageCostTable(costs, h.modelCatalog))
	if err != nil {
		h.writeInternalError(w, "failed to summarize usage", err)
		return
	}

	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) handleUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(adminUIHTML))
}

// ===== Target CRUD =====

func (h *Handler) handleListTargets(w http.ResponseWriter, r *http.Request) {
	cfg, err := h.currentConfig()
	if err != nil {
		h.writeInternalError(w, "failed to load config", err)
		return
	}

	targets := make([]map[string]any, len(cfg.AzureTargets))
	for i, t := range cfg.AzureTargets {
		epType := config.NormalizeEndpointType(t.EndpointType)
		targets[i] = map[string]any{
			"name":                     t.Name,
			"endpoint_type":            epType,
			"endpoint":                 t.Endpoint,
			"resource_path_prefix":     t.ResourcePathPrefix,
			"has_api_key":              t.AzureAPIKey != "",
			"allow_bearer_passthrough": t.AllowBearer,
			"allowed_models":           t.AllowedModels,
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

	for _, t := range cfg.AzureTargets {
		if strings.EqualFold(t.Name, name) {
			writeJSON(w, http.StatusConflict, errorResponse(fmt.Sprintf("target %q already exists", name)))
			return
		}
	}

	newTarget := config.AzureTarget{
		Name:               name,
		EndpointType:       epType,
		Endpoint:           endpoint,
		ResourcePathPrefix: rpp,
		AzureAPIKey:        apiKey,
		AllowBearer:        body.AllowBearer,
		AllowedModels:      body.AllowedModels,
	}
	cfg.AzureTargets = append(cfg.AzureTargets, newTarget)

	if err := h.saveConfig(cfg); err != nil {
		h.writeInternalError(w, "failed to save config", err)
		return
	}

	if err := h.applyConfigRuntime(cfg); err != nil {
		h.logger.Warn("target created but runtime apply failed", "error", err)
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
	for i := range cfg.AzureTargets {
		if strings.EqualFold(cfg.AzureTargets[i].Name, name) {
			t := &cfg.AzureTargets[i]
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
				t.AzureAPIKey = strings.TrimSpace(*body.APIKey)
			}
			t.AllowBearer = body.AllowBearer
			t.AllowedModels = body.AllowedModels

			// Validate: api_key must be non-empty when allow_bearer is false.
			if t.AzureAPIKey == "" && !t.AllowBearer {
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

	if err := h.saveConfig(cfg); err != nil {
		h.writeInternalError(w, "failed to save config", err)
		return
	}

	if err := h.applyConfigRuntime(cfg); err != nil {
		h.logger.Warn("target updated but runtime apply failed", "error", err)
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

	next := make([]config.AzureTarget, 0, len(cfg.AzureTargets))
	found := false
	for _, t := range cfg.AzureTargets {
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

	cfg.AzureTargets = next

	if err := h.saveConfig(cfg); err != nil {
		h.writeInternalError(w, "failed to save config", err)
		return
	}

	if err := h.applyConfigRuntime(cfg); err != nil {
		h.logger.Warn("target deleted but runtime apply failed", "error", err)
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
	clientStore := nosql.NewClientStore(cfg.DataFiles.ClientsFile)
	clients, err := clientStore.List()
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

func (h *Handler) currentClientStore() (*nosql.ClientStore, error) {
	cfg, err := h.currentConfig()
	if err != nil {
		return nil, err
	}
	return nosql.NewClientStore(cfg.DataFiles.ClientsFile), nil
}

func (h *Handler) currentModelCostStore() (*nosql.ModelCostStore, error) {
	cfg, err := h.currentConfig()
	if err != nil {
		return nil, err
	}
	return nosql.NewModelCostStore(cfg.DataFiles.ModelCostsFile), nil
}

func (h *Handler) currentUsageStore() (*usage.Store, error) {
	cfg, err := h.currentConfig()
	if err != nil {
		return nil, err
	}
	return usage.NewStore(cfg.DataFiles.UsageEventsFile), nil
}

func (h *Handler) reloadAuthFromClientStore(store *nosql.ClientStore) error {
	clients, err := store.List()
	if err != nil {
		return err
	}
	return h.authStore.LoadFromConfig(clients)
}

func (h *Handler) writeInternalError(w http.ResponseWriter, message string, err error) {
	h.logger.Error(message, "error", err)
	writeJSON(w, http.StatusInternalServerError, errorResponse("internal server error"))
}

func (h *Handler) listAuditEvents(limit int) ([]AuditEvent, error) {
	if h.auditStore == nil {
		return []AuditEvent{}, nil
	}
	return h.auditStore.List(limit)
}

func (h *Handler) listAuditEventsFromRequest(r *http.Request) ([]AuditEvent, error) {
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
	_ = h.auditStore.Record(AuditEvent{
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

// toUsageCostTable 构建费用查找表。
// 优先级：自定义 model_costs 覆盖 > catalog 内嵌默认价格。
func toUsageCostTable(costs []nosql.ModelCost, cat *catalog.Catalog) usage.CostTable {
	table := make(usage.CostTable)

	// 第一层：从 catalog 填充默认价格作为基线
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

	// 第二层：自定义 model_costs 覆盖（优先级更高）
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
