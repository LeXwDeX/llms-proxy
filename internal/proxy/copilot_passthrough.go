// copilot_passthrough.go — Copilot 透传路由：
// 下游以 github-proxy provider 身份连接，代理仅替换 Authorization Bearer token
// 后透传到 Copilot 上游，保留下游原始 headers，不做模型名映射。
// GitHub 上游强制要求 Editor headers，若下游未提供则由代理补充默认值。
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/ycgame/llms-proxy/internal/auth"
	"github.com/ycgame/llms-proxy/internal/copilot"
	appmiddleware "github.com/ycgame/llms-proxy/internal/middleware"
	"github.com/ycgame/llms-proxy/internal/nosql"
)

// ensureCopilotHeaders 确保 GitHub 上游必需的 Editor headers 存在。
// 如果下游已经提供了某个 header，保留下游的值（透传优先）；
// 如果缺失，补充代理默认值（源自 copilot.ApplyEditorHeaders）。
func ensureCopilotHeaders(h http.Header) {
	if h.Get("Editor-Version") == "" {
		h.Set("Editor-Version", copilot.HeaderEditorVersion)
	}
	if h.Get("Editor-Plugin-Version") == "" {
		h.Set("Editor-Plugin-Version", copilot.HeaderPluginVersion)
	}
	if h.Get("Copilot-Integration-Id") == "" {
		h.Set("Copilot-Integration-Id", copilot.HeaderIntegrationID)
	}
	if h.Get("User-Agent") == "" {
		h.Set("User-Agent", copilot.HeaderUserAgent)
	}
}

// copilotPassthroughSetup 是透传路由的共用 helper：
// 查找 Pool → 选号 → 获取 Token → 确定 baseURL。
// 出错时直接写 HTTP 错误响应，调用方检查 err != nil 后 return 即可。
func (s *Service) copilotPassthroughSetup(
	w http.ResponseWriter,
	r *http.Request,
	principal *auth.Principal,
) (account *nosql.CopilotAccount, token string, baseURL string, err error) {
	requestID := appmiddleware.RequestIDFromContext(r.Context())

	if s.copilotService == nil {
		http.Error(w, "copilot service not configured", http.StatusBadGateway)
		return nil, "", "", fmt.Errorf("copilot service not configured")
	}

	pool, poolErr := s.copilotService.FindPoolByClient(principal.Name)
	if poolErr != nil {
		s.logger.Warn("copilot passthrough: pool not found",
			"request_id", requestID,
			"client", principal.Name,
			"error", poolErr,
		)
		http.Error(w, "copilot pool not found for client", http.StatusBadRequest)
		return nil, "", "", poolErr
	}

	account, selectErr := s.copilotService.SelectAccount(pool.Name, "")
	if selectErr != nil {
		s.logger.Warn("copilot passthrough: no available account",
			"request_id", requestID,
			"pool", pool.Name,
			"error", selectErr,
		)
		http.Error(w, "no available copilot account: "+selectErr.Error(), http.StatusServiceUnavailable)
		return nil, "", "", selectErr
	}

	token, tokenErr := s.copilotService.GetToken(r.Context(), account.ID)
	if tokenErr != nil {
		s.logger.Error("copilot passthrough: get token failed",
			"request_id", requestID,
			"account_id", account.ID,
			"error", tokenErr,
		)
		http.Error(w, "failed to get copilot token", http.StatusBadGateway)
		return nil, "", "", tokenErr
	}

	baseURL = copilot.CopilotIndividualBase
	if account.APIBaseURL != "" {
		baseURL = strings.TrimRight(account.APIBaseURL, "/")
	}

	return account, token, baseURL, nil
}

// HandleCopilotAuth 处理 GET /copilot/auth —— 返回客户端的 Copilot 可用性信息。
func (s *Service) HandleCopilotAuth(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.PrincipalFromContext(r.Context())
	if !ok || principal == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	if s.copilotService == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "copilot service not configured",
		})
		return
	}

	pool, err := s.copilotService.FindPoolByClient(principal.Name)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "copilot pool not found for client",
		})
		return
	}

	accounts, err := s.copilotService.GetAccountStore().ListByPool(pool.Name)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "failed to list accounts",
		})
		return
	}

	available := 0
	for _, a := range accounts {
		if a.Status == nosql.AccountStatusActive || a.Status == nosql.AccountStatusQuotaExceeded {
			available++
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status":             "ok",
		"client":             principal.Name,
		"pool":               pool.Name,
		"accounts_available": available,
	})
}

// HandleCopilotQuotaSummary 处理 GET /copilot/quota —— 汇总该客户端所在池的 premium request 配额。
// 仅对已认证客户端可见，返回跨所有账户的剩余和总额之和，以及账号数统计。
//
// 响应格式：
//
//	{
//	  "remaining":        133,   // 池内所有活跃账户剩余 premium requests 之和
//	  "entitlement":      300,   // 池内所有活跃账户月度总额度之和
//	  "accounts_active":  1,     // 活跃（active + quota_exceeded）账户数
//	  "accounts_total":   2      // 池内全部账户数（含 disabled/error 等）
//	}
//
// 客户端可据此显示 [accounts_active/accounts_total | remaining/entitlement]。
func (s *Service) HandleCopilotQuotaSummary(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.PrincipalFromContext(r.Context())
	if !ok || principal == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	if s.copilotService == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "copilot service not configured",
		})
		return
	}

	pool, err := s.copilotService.FindPoolByClient(principal.Name)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "copilot pool not found for client",
		})
		return
	}

	accounts, err := s.copilotService.GetAccountStore().ListByPool(pool.Name)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "failed to list accounts",
		})
		return
	}

	totalRemaining := 0
	totalEntitlement := 0
	active := 0
	for _, a := range accounts {
		if a.Status != nosql.AccountStatusActive && a.Status != nosql.AccountStatusQuotaExceeded {
			continue
		}
		active++
		totalRemaining += a.QuotaRemaining
		totalEntitlement += a.QuotaEntitlement
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"remaining":       totalRemaining,
		"entitlement":     totalEntitlement,
		"accounts_active": active,
		"accounts_total":  len(accounts),
	})
}

// HandleCopilotModels 处理 GET /copilot/models —— 透传上游 models 列表。
func (s *Service) HandleCopilotModels(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.PrincipalFromContext(r.Context())
	if !ok || principal == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	account, token, baseURL, err := s.copilotPassthroughSetup(w, r, principal)
	if err != nil {
		return
	}

	requestID := appmiddleware.RequestIDFromContext(r.Context())
	upstreamURL := baseURL + "/models"

	ctx, cancel := context.WithTimeout(r.Context(), s.getRequestTimeout())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstreamURL, nil)
	if err != nil {
		s.logger.Error("copilot models: create request failed",
			"request_id", requestID,
			"error", err,
		)
		http.Error(w, "failed to create upstream request", http.StatusInternalServerError)
		return
	}

	copyHeaders(req.Header, r.Header)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Del("api-key")
	ensureCopilotHeaders(req.Header)
	// 列模型属工具行为；body 为 nil 时 inferInitiator 兜底返回 "agent"，
	// 客户端如显式声明合法值（user/agent）则尊重。
	req.Header.Set("X-Initiator", inferInitiator(nil, r.Header.Get("X-Initiator")))

	resp, err := s.httpClient.Do(req)
	if err != nil {
		s.logger.Error("copilot models: upstream request failed",
			"request_id", requestID,
			"error", err,
		)
		http.Error(w, "copilot upstream error", http.StatusBadGateway)
		s.metrics.totalFailures.Add(1)
		return
	}
	defer resp.Body.Close()

	// 透传响应
	w.Header().Set("X-Copilot-Account", account.GitHubUsername)
	w.Header().Set("X-Copilot-Quota-Remaining", fmt.Sprintf("%.1f", account.QuotaPercentRemaining))

	for key, values := range resp.Header {
		if _, skip := hopHeaders[key]; skip {
			continue
		}
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	writer := newStreamingWriter(w)
	_, _ = io.Copy(writer, resp.Body)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		s.metrics.totalSuccess.Add(1)
	} else {
		s.metrics.totalFailures.Add(1)
	}
}

// HandleCopilotPassthrough 处理 /copilot/* —— catch-all 透传。
// 去除 /copilot 前缀后直接透传到上游，仅替换 Authorization token。
func (s *Service) HandleCopilotPassthrough(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.PrincipalFromContext(r.Context())
	if !ok || principal == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	requestID := appmiddleware.RequestIDFromContext(r.Context())

	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.logger.Error("copilot passthrough: read body failed",
			"request_id", requestID,
			"error", err,
		)
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}

	account, token, baseURL, setupErr := s.copilotPassthroughSetup(w, r, principal)
	if setupErr != nil {
		return
	}

	// 诊断日志：检查请求体中 thinking block 的 signature 完整性
	if len(body) > 0 && strings.Contains(r.URL.Path, "/messages") {
		logThinkingBlockDiagnostics(s, requestID, body, "passthrough-received")
	}

	// 诊断日志：记录下游送的 x-initiator 与代理决定的 x-initiator 对照，
	// 以及模型名称，用于排查 Copilot premium-request 扣次异常。
	downstreamInitiator := r.Header.Get("X-Initiator")
	proxyInitiator := inferInitiator(body, downstreamInitiator)
	reqModel := extractModelFromBody(body)
	if r.Method == http.MethodPost {
		s.logger.Info("copilot passthrough billing-diag",
			"request_id", requestID,
			"client", principal.Name,
			"downstream_initiator", downstreamInitiator,
			"proxy_initiator", proxyInitiator,
			"model", reqModel,
			"path", r.URL.Path,
		)
	}

	// 路径映射：去除 /copilot 前缀
	upstreamPath := strings.TrimPrefix(r.URL.Path, "/copilot")
	if upstreamPath == "" {
		upstreamPath = "/"
	}

	upstreamURL := baseURL + upstreamPath
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.getRequestTimeout())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, r.Method, upstreamURL, bytes.NewReader(body))
	if err != nil {
		s.logger.Error("copilot passthrough: create request failed",
			"request_id", requestID,
			"error", err,
		)
		http.Error(w, "failed to create upstream request", http.StatusInternalServerError)
		s.metrics.totalFailures.Add(1)
		return
	}

	// 复制所有下游 headers
	copyHeaders(req.Header, r.Header)

	// 仅替换 auth
	req.Header.Set("Authorization", "Bearer "+token)

	// 删除代理内部 headers
	req.Header.Del("api-key")
	req.Header.Del(headerProxyTarget)
	req.Header.Del(headerAzureAuthorization)

	// 确保 GitHub 必需的 Editor headers 存在
	ensureCopilotHeaders(req.Header)

	// 注入 X-Initiator：客户端传了合法值则尊重，否则按 body 推断（兜底 agent）。
	req.Header.Set("X-Initiator", proxyInitiator)

	// 设置 body
	if len(body) > 0 {
		req.ContentLength = int64(len(body))
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		s.logger.Error("copilot passthrough: upstream request failed",
			"request_id", requestID,
			"account_id", account.ID,
			"error", err,
		)
		http.Error(w, "copilot upstream error", http.StatusBadGateway)
		s.metrics.totalFailures.Add(1)
		return
	}
	defer resp.Body.Close()

	// 诊断：4xx 响应记录上游错误详情（含 "Invalid signature" 等）
	logUpstream4xx(s, resp, requestID, r.URL.Path, upstreamPath)

	// 透传响应
	w.Header().Set("X-Copilot-Account", account.GitHubUsername)
	w.Header().Set("X-Copilot-Quota-Remaining", fmt.Sprintf("%.1f", account.QuotaPercentRemaining))

	for key, values := range resp.Header {
		if _, skip := hopHeaders[key]; skip {
			continue
		}
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	writer := newStreamingWriter(w)
	_, _ = io.Copy(writer, resp.Body)

	// 记录 metrics
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		s.metrics.totalSuccess.Add(1)
	} else {
		s.metrics.totalFailures.Add(1)
	}

	s.logger.Info("copilot passthrough completed",
		"request_id", requestID,
		"client", principal.Name,
		"account", account.GitHubUsername,
		"method", r.Method,
		"path", r.URL.Path,
		"upstream_path", upstreamPath,
		"status", resp.StatusCode,
	)
}

// extractModelFromBody 从请求体 JSON 中提取 "model" 字段值。
func extractModelFromBody(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var partial struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &partial); err != nil {
		return ""
	}
	return partial.Model
}

// logThinkingBlockDiagnostics 解析请求体中 messages 数组，找出所有 thinking block，
// 记录 signature 长度、前 40 字符和 thinking 文本长度，用于诊断
// "Invalid signature in thinking block" 错误。
func logThinkingBlockDiagnostics(s *Service, requestID string, body []byte, phase string) {
	var payload struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return
	}
	for i, msg := range payload.Messages {
		if msg.Role != "assistant" {
			continue
		}
		var blocks []struct {
			Type      string `json:"type"`
			Thinking  string `json:"thinking"`
			Signature string `json:"signature"`
			Data      string `json:"data"` // redacted_thinking
		}
		if err := json.Unmarshal(msg.Content, &blocks); err != nil {
			continue
		}
		for j, block := range blocks {
			if block.Type != "thinking" && block.Type != "redacted_thinking" {
				continue
			}
			sigPreview := block.Signature
			if len(sigPreview) > 40 {
				sigPreview = sigPreview[:40] + "..."
			}
			s.logger.Info("thinking-block-diag",
				"phase", phase,
				"request_id", requestID,
				"msg_index", i,
				"block_index", j,
				"block_type", block.Type,
				"thinking_len", len(block.Thinking),
				"signature_len", len(block.Signature),
				"signature_preview", sigPreview,
				"redacted_data_len", len(block.Data),
				"body_total_len", len(body),
			)
		}
	}
}

// logUpstream4xx 在 /messages 路径遇到 4xx 时，读取并记录上游错误体（最多 1KB），
// 然后还原 resp.Body 以便下游正常读取。
func logUpstream4xx(s *Service, resp *http.Response, requestID, path, upstreamPath string) {
	if resp.StatusCode < 400 || resp.StatusCode >= 500 {
		return
	}
	errBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if readErr != nil {
		return
	}
	preview := string(errBody)
	if len(preview) > 1024 {
		preview = preview[:1024] + "...(truncated)"
	}
	s.logger.Warn("copilot upstream 4xx",
		"request_id", requestID,
		"status", resp.StatusCode,
		"path", path,
		"upstream_path", upstreamPath,
		"error_body", preview,
	)
	resp.Body = io.NopCloser(bytes.NewReader(errBody))
}
