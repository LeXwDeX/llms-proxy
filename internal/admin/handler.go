package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"log/slog"

	"github.com/ycgame/azure-proxy/internal/auth"
	"github.com/ycgame/azure-proxy/internal/config"
	"github.com/ycgame/azure-proxy/internal/nosql"
	"github.com/ycgame/azure-proxy/internal/proxy"
	"github.com/ycgame/azure-proxy/internal/usage"
)

// Handler wires administration endpoints.
type Handler struct {
	configManager *config.Manager
	authStore     *auth.Store
	proxyService  *proxy.Service
	auditStore    *AuditStore
	logger        *slog.Logger
}

// NewHandler constructs an HTTP router exposing admin endpoints.
func NewHandler(
	manager *config.Manager,
	store *auth.Store,
	service *proxy.Service,
	auditStore *AuditStore,
	logger *slog.Logger,
) http.Handler {
	h := &Handler{
		configManager: manager,
		authStore:     store,
		proxyService:  service,
		auditStore:    auditStore,
		logger:        logger,
	}

	r := chi.NewRouter()
	r.Get("/", h.handleUI)
	r.Get("/ui", h.handleUI)
	r.Get("/healthz", h.handleHealthz)
	r.Get("/metrics", h.handleMetrics)
	r.Get("/api/me", h.handleMe)
	r.Get("/api/overview", h.handleOverview)
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
	summary, err := usageStore.Summary(now, toUsageCostTable(costs))
	if err != nil {
		h.writeInternalError(w, "failed to summarize usage", err)
		return
	}

	response := map[string]any{
		"generated_at":    now,
		"targets":         health,
		"target_count":    len(health),
		"active_requests": metrics.ActiveRequests,
		"requests": map[string]int64{
			"total":    metrics.TotalRequests,
			"success":  metrics.TotalSuccess,
			"failures": metrics.TotalFailures,
			"retries":  metrics.TotalRetries,
		},
		"clients":     len(clients),
		"model_costs": len(models),
		"usage":       summary,
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
	h.recordAudit(r, "client_create", req.Name, "success", req.AccessKey)

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
	h.recordAudit(r, "client_update", name, "success", req.AccessKey)

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
		InputPer1KTokens      float64 `json:"input_per_1k_tokens"`
		OutputPer1KTokens     float64 `json:"output_per_1k_tokens"`
		CachedInputPer1KToken float64 `json:"cached_input_per_1k_tokens"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse("invalid json body"))
		return
	}

	if err := store.Upsert(nosql.ModelCost{
		Model:                 model,
		InputPer1KTokens:      req.InputPer1KTokens,
		OutputPer1KTokens:     req.OutputPer1KTokens,
		CachedInputPer1KToken: req.CachedInputPer1KToken,
	}); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(err.Error()))
		return
	}
	h.recordAudit(r, "model_cost_upsert", model, "success", "")

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
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

	if err := store.Delete(model); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		writeJSON(w, status, errorResponse(err.Error()))
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

	result, err := usageStore.Aggregate(filter, r.URL.Query().Get("group_by"), toUsageCostTable(costs))
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

	result, err := usageStore.Summary(time.Now().UTC(), toUsageCostTable(costs))
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

func toUsageCostTable(costs []nosql.ModelCost) usage.CostTable {
	table := make(usage.CostTable, len(costs))
	for _, cost := range costs {
		key := strings.ToLower(strings.TrimSpace(cost.Model))
		if key == "" {
			continue
		}
		table[key] = usage.CostRates{
			InputPer1KTokens:      cost.InputPer1KTokens,
			OutputPer1KTokens:     cost.OutputPer1KTokens,
			CachedInputPer1KToken: cost.CachedInputPer1KToken,
		}
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
