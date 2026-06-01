package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ycgame/llms-proxy/internal/config"
	"github.com/ycgame/llms-proxy/internal/copilot"
	"github.com/ycgame/llms-proxy/internal/nosql"
)

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

// validateCopilotTargets 校验所有 target 名称存在。
// Copilot 是独立模块，不再校验 endpoint_type。
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
		if _, ok := targetMap[name]; !ok {
			return fmt.Errorf("target %q not found", tName)
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
		var noCopilot *copilot.NoCopilotAccessError
		switch {
		case errors.As(err, &noCopilot):
			// 获取 username 用于展示（CompleteAuth 已提前保存）
			displayName := noCopilot.GitHubUserID
			if acct, lookupErr := h.copilotAcctStore.Get(id); lookupErr == nil && acct.GitHubUsername != "" {
				displayName = acct.GitHubUsername
			}
			msg := fmt.Sprintf("该 GitHub 账户 %s 未开通 Copilot 订阅，请先开通后再添加", displayName)
			writeJSON(w, http.StatusForbidden, errorResponse(msg))
		case errors.Is(err, copilot.ErrDeviceCodeExpired):
			writeJSON(w, http.StatusBadRequest, errorResponse("授权码已过期，请重新发起授权"))
		case errors.Is(err, copilot.ErrAccessDenied):
			writeJSON(w, http.StatusForbidden, errorResponse("用户拒绝了授权请求"))
		default:
			h.writeInternalError(w, "failed to complete copilot auth", err)
		}
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

func (h *Handler) handleToggleCopilotOverage(w http.ResponseWriter, r *http.Request) {
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

	account.AllowOverage = !account.AllowOverage
	if err := h.copilotAcctStore.Update(id, *account); err != nil {
		h.writeInternalError(w, "failed to toggle overage", err)
		return
	}

	detail := "allow_overage=" + fmt.Sprintf("%v", account.AllowOverage)
	h.recordAudit(r, "copilot.account.toggle_overage", id, "success", detail)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":        "ok",
		"allow_overage": account.AllowOverage,
	})
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
		"quota_entitlement":       account.QuotaEntitlement,
		"quota_remaining":         account.QuotaRemaining,
		"quota_reset_at":          account.QuotaResetAt,
		"quota_last_sync_at":      account.QuotaLastSyncAt,
		"quota_billing_model":     account.QuotaBillingModel,
		"quota_unlimited":         account.QuotaUnlimited,
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
	account.QuotaUnlimited = quotaInfo.Unlimited
	if quotaInfo.ResetAt != "" {
		account.QuotaResetAt = quotaInfo.ResetAt
	}
	account.QuotaLastSyncAt = time.Now().UTC().Format(time.RFC3339)
	// unlimited 账户 entitlement/remaining 为 0，直接覆盖清除旧值
	account.QuotaEntitlement = quotaInfo.Entitlement
	account.QuotaRemaining = quotaInfo.Remaining
	account.QuotaBillingModel = quotaInfo.BillingModel

	if err := h.copilotAcctStore.Update(id, *account); err != nil {
		h.writeInternalError(w, "failed to update copilot account quota", err)
		return
	}

	auditDetail := fmt.Sprintf("remaining=%.1f%% (%d/%d)", quotaInfo.PercentRemaining, quotaInfo.Remaining, quotaInfo.Entitlement)
	if quotaInfo.Unlimited {
		auditDetail = "unlimited"
	}
	h.recordAudit(r, "copilot.quota.sync", id, "success", auditDetail)
	writeJSON(w, http.StatusOK, map[string]any{
		"account_id":              id,
		"quota_percent_remaining": quotaInfo.PercentRemaining,
		"quota_entitlement":       quotaInfo.Entitlement,
		"quota_remaining":         quotaInfo.Remaining,
		"quota_reset_at":          quotaInfo.ResetAt,
		"quota_last_sync_at":      account.QuotaLastSyncAt,
		"copilot_plan":            quotaInfo.CopilotPlan,
		"quota_billing_model":     quotaInfo.BillingModel,
		"quota_unlimited":         quotaInfo.Unlimited,
	})
}

func (h *Handler) handleListCopilotModels(w http.ResponseWriter, r *http.Request) {
	// 尝试通过缓存获取完整模型元数据
	if h.copilotService != nil {
		models, err := h.copilotService.GetCachedModels(r.Context())
		if err == nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"models": models,
				"count":  len(models),
				"source": "copilot_api",
			})
			return
		}
		h.logger.Warn("从缓存获取 Copilot 模型失败，降级为本地列表", "error", err)
	}

	// 降级：返回本地硬编码乘数表
	models := copilot.ListAvailableModels()
	writeJSON(w, http.StatusOK, map[string]any{
		"models": models,
		"count":  len(models),
		"source": "local",
	})
}

// handleListCopilotPoolModels 获取指定池内所有账户可用模型的交集。
// GET /admin/data/copilot-pools/{name}/models
// 逻辑：遍历池内所有 active/quota_exceeded 账户，每个账户通过 Copilot API 获取模型列表，
// 最终返回所有账户的交集（只有所有账户都有的模型才返回）。
func (h *Handler) handleListCopilotPoolModels(w http.ResponseWriter, r *http.Request) {
	poolName := strings.TrimSpace(chi.URLParam(r, "name"))
	if poolName == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("missing pool name"))
		return
	}

	if h.copilotService == nil || h.copilotAcctStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse("copilot not configured"))
		return
	}

	accounts, err := h.copilotAcctStore.ListByPool(poolName)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse("list accounts: "+err.Error()))
		return
	}

	// 收集每个可用账户的模型列表
	type acctModels struct {
		AccountID string
		Username  string
		Models    []copilot.CopilotModelDetail
	}
	var perAccount []acctModels

	for _, acct := range accounts {
		if (acct.Status != nosql.AccountStatusActive && acct.Status != nosql.AccountStatusQuotaExceeded) || acct.OAuthToken == "" {
			continue
		}

		// P2-7: 通过 service 层封装获取模型列表
		models, err := h.copilotService.FetchAccountModels(r.Context(), acct.ID)
		if err != nil {
			h.logger.Warn("获取账户模型列表失败，跳过",
				"pool", poolName, "account_id", acct.ID, "error", err)
			continue
		}

		perAccount = append(perAccount, acctModels{
			AccountID: acct.ID,
			Username:  acct.GitHubUsername,
			Models:    models,
		})
	}

	if len(perAccount) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"pool":          poolName,
			"models":        []copilot.CopilotModelDetail{},
			"count":         0,
			"account_count": 0,
			"source":        "no_available_accounts",
		})
		return
	}

	// 计算交集：以第一个账户的模型为基准，保留所有账户都有的模型
	intersection := make(map[string]copilot.CopilotModelDetail)
	for _, m := range perAccount[0].Models {
		intersection[m.ID] = m
	}

	for i := 1; i < len(perAccount); i++ {
		otherSet := make(map[string]bool)
		for _, m := range perAccount[i].Models {
			otherSet[m.ID] = true
		}
		for id := range intersection {
			if !otherSet[id] {
				delete(intersection, id)
			}
		}
	}

	// 转为有序切片
	var result []copilot.CopilotModelDetail
	for _, m := range intersection {
		result = append(result, m)
	}
	// P2-9: 使用 copilot.SortModelDetailByCategory 替代冒泡排序
	copilot.SortModelDetailByCategory(result)

	writeJSON(w, http.StatusOK, map[string]any{
		"pool":          poolName,
		"models":        result,
		"count":         len(result),
		"account_count": len(perAccount),
		"source":        "copilot_api_intersection",
	})
}

// handleRefreshCopilotToken 强制清空并重新获取账户的 Copilot Token。
// 常用场景：账户加入组织 Business seat 后，旧 Token 记录了 individual 端点，
// 调用此接口可强制刷新，使 api_base_url 更新为正确的 business 端点。
func (h *Handler) handleRefreshCopilotToken(w http.ResponseWriter, r *http.Request) {
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

	// 检查 OAuthToken 是否存在
	if account.OAuthToken == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse("账户缺少 OAuth token，请先完成授权"))
		return
	}

	// 清空旧 Token，强制 EnsureValidToken 重新向 GitHub 获取
	account.CopilotToken = ""
	account.CopilotTokenExpiresAt = 0
	if err := h.copilotAcctStore.Update(id, *account); err != nil {
		h.writeInternalError(w, "failed to clear copilot token", err)
		return
	}

	// 立即触发一次刷新，绕过状态检查直接使用 TokenManager
	// 允许 token_expired/disabled 状态的账户也能刷新 Token 并恢复为 active
	if _, err := h.copilotService.ForceRefreshToken(r.Context(), id); err != nil {
		writeJSON(w, http.StatusBadGateway, errorResponse("token refresh failed: "+err.Error()))
		return
	}

	// 读回更新后的账户，返回最新 api_base_url
	updated, err := h.copilotAcctStore.Get(id)
	if err != nil {
		h.writeInternalError(w, "failed to read updated account", err)
		return
	}

	h.recordAudit(r, "copilot.account.refresh_token", id, "success", "api_base_url="+updated.APIBaseURL)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":       "ok",
		"api_base_url": updated.APIBaseURL,
	})
}
