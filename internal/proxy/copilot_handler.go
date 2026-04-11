// copilot_handler.go — Copilot 专用请求处理：
// Pool 查找 → 顺序选号 → 动态 Token 注入 → 模型名映射 → 转发 → 额度扣减。
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/ycgame/llms-proxy/internal/auth"
	"github.com/ycgame/llms-proxy/internal/copilot"
	appmiddleware "github.com/ycgame/llms-proxy/internal/middleware"
	"github.com/ycgame/llms-proxy/internal/nosql"
)

// handleCopilotRequest 处理 Copilot 类型的请求。
// 完整链路：查找 Pool → 顺序选号 → 获取 Token → 模型名映射 → 构建上游请求 → 转发 → 额度扣减。
func (s *Service) handleCopilotRequest(
	w http.ResponseWriter,
	r *http.Request,
	principal *auth.Principal,
	body []byte,
	model string,
) {
	requestID := appmiddleware.RequestIDFromContext(r.Context())

	// 1. 根据 client name 查找 Pool
	pool, err := s.findPoolByClient(principal.Name)
	if err != nil {
		s.logger.Error("查找 Copilot Pool 失败",
			"request_id", requestID,
			"client", principal.Name,
			"error", err,
		)
		http.Error(w, "copilot pool not found for client", http.StatusBadRequest)
		s.metrics.totalFailures.Add(1)
		return
	}

	// 2. 顺序选号：找到第一个 active 且有额度的账户
	account, err := s.selectCopilotAccount(pool.Name, model)
	if err != nil {
		s.logger.Warn("无可用 Copilot 账户",
			"request_id", requestID,
			"pool", pool.Name,
			"error", err,
		)
		http.Error(w, "no available copilot account: "+err.Error(), http.StatusServiceUnavailable)
		s.metrics.totalFailures.Add(1)
		return
	}

	s.logger.Info("Copilot 选号完成",
		"request_id", requestID,
		"pool", pool.Name,
		"account_id", account.ID,
		"username", account.GitHubUsername,
		"sort_order", account.SortOrder,
		"quota_remaining", account.QuotaPercentRemaining,
	)

	// 3. 获取有效的 Copilot access token
	token, err := s.copilotService.GetToken(r.Context(), account.ID)
	if err != nil {
		s.logger.Error("获取 Copilot token 失败",
			"request_id", requestID,
			"account_id", account.ID,
			"error", err,
		)
		http.Error(w, "failed to get copilot token", http.StatusBadGateway)
		s.metrics.totalFailures.Add(1)
		return
	}

	// 4. 模型名映射：copilot_xxx → xxx
	upstreamModel := model
	if mapped, found := copilot.MapModelName(model); found {
		upstreamModel = mapped
	}

	// 5. 替换请求体中的 model 字段
	forwardBody := body
	if upstreamModel != model {
		forwardBody = replaceModelInBody(body, upstreamModel)
	}

	// 6. 构建上游请求（使用账户动态 API 端点 + 客户端原始路径）
	// 上游路径跟随客户端请求路径，不强制改写为 /chat/completions，
	// 使得 /responses、/embeddings 等路径可以直接透传给 Copilot 上游。
	//
	// Copilot 上游路径规则不统一：
	//   - /chat/completions : 不接受 /v1 前缀（/v1/chat/completions → 404）
	//   - /responses        : 有无 /v1 均可
	//   - /v1/messages      : 必须保留 /v1 前缀（/messages → 404，Anthropic 原生格式）
	// 因此仅对非 /v1/messages 路径去除 /v1 前缀。
	baseURL := copilot.CopilotIndividualBase
	if account.APIBaseURL != "" {
		baseURL = strings.TrimRight(account.APIBaseURL, "/")
	}
	requestPath := r.URL.Path
	if !strings.HasPrefix(requestPath, "/v1/messages") {
		requestPath = strings.TrimPrefix(requestPath, "/v1")
	}
	if requestPath == "" {
		requestPath = "/chat/completions"
	}
	upstreamURL := baseURL + requestPath
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.getRequestTimeout())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, r.Method, upstreamURL, bytes.NewReader(forwardBody))
	if err != nil {
		s.logger.Error("创建上游请求失败",
			"request_id", requestID,
			"error", err,
		)
		http.Error(w, "failed to create upstream request", http.StatusInternalServerError)
		s.metrics.totalFailures.Add(1)
		return
	}

	// 复制 headers（排除 hop-by-hop 和认证相关）
	copyHeaders(req.Header, r.Header)
	req.Header.Del("Authorization")
	req.Header.Del(headerProxyTarget)
	req.Header.Del(headerAzureAuthorization)
	req.Header.Del("api-key")

	// 设置 Copilot 认证和 Editor Headers
	req.Header.Set("Authorization", "Bearer "+token)
	copilot.ApplyEditorHeaders(req)
	req.Header.Set("Content-Type", "application/json")

	if len(forwardBody) > 0 {
		req.ContentLength = int64(len(forwardBody))
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(forwardBody)), nil
		}
	}

	// 7. 发送请求
	resp, err := s.httpClient.Do(req)
	if err != nil {
		s.logger.Error("Copilot 上游请求失败",
			"request_id", requestID,
			"account_id", account.ID,
			"error", err,
		)
		http.Error(w, "copilot upstream error", http.StatusBadGateway)
		s.metrics.totalFailures.Add(1)
		return
	}
	defer resp.Body.Close()

	// 8. 成功响应后扣减额度
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		copilot.DeductQuota(account, upstreamModel)
		if updateErr := s.copilotAcctStore.Update(account.ID, *account); updateErr != nil {
			s.logger.Warn("额度扣减写回失败",
				"request_id", requestID,
				"account_id", account.ID,
				"error", updateErr,
			)
		}

		// 如果额度耗尽，更新状态
		if copilot.IsQuotaExhausted(account) {
			account.Status = nosql.AccountStatusQuotaExceeded
			if updateErr := s.copilotAcctStore.Update(account.ID, *account); updateErr != nil {
				s.logger.Warn("额度耗尽状态更新失败",
					"request_id", requestID,
					"account_id", account.ID,
					"error", updateErr,
				)
			}
		}
		s.metrics.totalSuccess.Add(1)
	} else {
		s.metrics.totalFailures.Add(1)
	}

	// 9. 写回响应
	for key, values := range resp.Header {
		if _, skip := hopHeaders[strings.ToLower(key)]; skip {
			continue
		}
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
	w.Header().Set("X-Copilot-Account", account.GitHubUsername)
	w.Header().Set("X-Copilot-Quota-Remaining", fmt.Sprintf("%.1f", account.QuotaPercentRemaining))
	w.WriteHeader(resp.StatusCode)

	// 使用 streaming writer 保证 SSE 实时刷新
	writer := newStreamingWriter(w)
	_, _ = io.Copy(writer, resp.Body)
}

// findPoolByClient 根据 client name 查找 CopilotPool。
func (s *Service) findPoolByClient(clientName string) (*nosql.CopilotPool, error) {
	pools, err := s.copilotPoolStore.List()
	if err != nil {
		return nil, fmt.Errorf("列出 pools: %w", err)
	}

	clientLower := strings.ToLower(strings.TrimSpace(clientName))
	for i := range pools {
		if strings.ToLower(strings.TrimSpace(pools[i].ClientName)) == clientLower {
			return &pools[i], nil
		}
	}

	return nil, fmt.Errorf("未找到 client %q 绑定的 copilot pool", clientName)
}

// selectCopilotAccount 按 SortOrder 顺序选择最佳可用 Copilot 账户。
// 选号策略（按优先级）：
//  1. active 且有额度的账户（最优）
//  2. 已开启「允许超额」的 active 或 quota_exceeded 账户（Copilot Business 超额仍可用）
//
// 免费模型不消耗额度，任何 active/quota_exceeded 账户均可。
func (s *Service) selectCopilotAccount(poolName, model string) (*nosql.CopilotAccount, error) {
	accounts, err := s.copilotAcctStore.ListByPool(poolName)
	if err != nil {
		return nil, fmt.Errorf("列出 pool %q 的账户: %w", poolName, err)
	}

	if len(accounts) == 0 {
		return nil, errors.New("pool 内无任何账户")
	}

	isFree := copilot.IsFreeModel(model)

	// accounts 已按 SortOrder 升序排列（ListByPool 保证）
	// 免费模型：直接返回第一个可用账户
	if isFree {
		for i := range accounts {
			a := &accounts[i]
			if a.Status == nosql.AccountStatusActive || a.Status == nosql.AccountStatusQuotaExceeded {
				return a, nil
			}
		}
		return nil, fmt.Errorf("pool %q 内无可用账户", poolName)
	}

	// 付费模型：两轮扫描
	// 第一轮：优先选 active 且有额度
	for i := range accounts {
		a := &accounts[i]
		if a.Status == nosql.AccountStatusActive && !copilot.IsQuotaExhausted(a) {
			return a, nil
		}
	}
	// 第二轮：选已开启「允许超额」的 active 或 quota_exceeded 账户
	for i := range accounts {
		a := &accounts[i]
		if a.AllowOverage && (a.Status == nosql.AccountStatusActive || a.Status == nosql.AccountStatusQuotaExceeded) {
			return a, nil
		}
	}

	return nil, fmt.Errorf("pool %q 内无可用账户（额度已耗尽且未开启超额调用）", poolName)
}

// replaceModelInBody 替换请求体 JSON 中的 "model" 字段值。
func replaceModelInBody(body []byte, newModel string) []byte {
	if len(body) == 0 {
		return body
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return body // 解析失败返回原始 body
	}

	if _, ok := parsed["model"]; ok {
		parsed["model"] = newModel
		modified, err := json.Marshal(parsed)
		if err != nil {
			return body
		}
		return modified
	}

	return body
}
