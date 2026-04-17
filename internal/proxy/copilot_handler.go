// copilot_handler.go — Copilot 专用请求处理：
// Pool 查找 → 顺序选号 → 动态 Token 注入 → 模型名映射 → 转发 → 额度扣减。
//
// Copilot 上游统一使用 OpenAI 兼容的 /chat/completions 端点，
// 无论底层模型是 GPT、Claude 还是 Gemini。
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
)

// copilotPathRules 定义 Copilot 上游路径前缀处理规则。
// 匹配顺序有意义：前面的规则优先级更高。
// 使用 slice 而非 map，因为需要前缀匹配的顺序优先级。
var copilotPathRules = []struct {
	Prefix  string // 客户端请求路径前缀
	StripV1 bool   // 是否去除 /v1 前缀
}{
	{Prefix: "/v1/", StripV1: true}, // /v1 路径：去除 /v1 前缀
}

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
	pool, err := s.copilotService.FindPoolByClient(principal.Name)
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
	account, err := s.copilotService.SelectAccount(pool.Name, model)
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

	// 4. 模型名映射：Copilot xxx → xxx
	upstreamModel := model
	if mapped, found := copilot.MapModelName(model); found {
		upstreamModel = mapped
	}

	// 5. 替换请求体中的 model 名
	forwardBody := body
	if upstreamModel != model {
		forwardBody = replaceModelInBody(body, upstreamModel)
	}

	// 6. 构建上游请求 URL
	baseURL := copilot.CopilotIndividualBase
	if account.APIBaseURL != "" {
		baseURL = strings.TrimRight(account.APIBaseURL, "/")
	}

	requestPath := r.URL.Path
	for _, rule := range copilotPathRules {
		if strings.HasPrefix(requestPath, rule.Prefix) {
			if rule.StripV1 {
				requestPath = strings.TrimPrefix(requestPath, "/v1")
			}
			break
		}
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

	// 8. 记录指标（不做本地额度扣减——GitHub 按 interaction 计费，
	//    agent 自主的后续 LLM 调用不算 premium request，
	//    本地逐请求扣减会严重高估消耗。额度由 QuotaManager 定期同步 GitHub API 获取真实值。）
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		s.metrics.totalSuccess.Add(1)
	} else {
		s.metrics.totalFailures.Add(1)
	}

	// 9. 透传响应
	w.Header().Set("X-Copilot-Account", account.GitHubUsername)
	w.Header().Set("X-Copilot-Quota-Remaining", fmt.Sprintf("%.1f", account.QuotaPercentRemaining))

	for key, values := range resp.Header {
		if _, skip := hopHeaders[strings.ToLower(key)]; skip {
			continue
		}
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	writer := newStreamingWriter(w)
	_, _ = io.Copy(writer, resp.Body)
}

// replaceModelInBody 替换请求体 JSON 中的 "model" 字段值。
// 使用 json.RawMessage 保留其他字段的原始字节，避免重新序列化破坏
// thinking block 中的 signature 等二进制/编码敏感字段。
func replaceModelInBody(body []byte, newModel string) []byte {
	if len(body) == 0 {
		return body
	}

	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(body, &parsed); err != nil {
		return body // 解析失败返回原始 body
	}

	if _, ok := parsed["model"]; ok {
		modelJSON, err := json.Marshal(newModel)
		if err != nil {
			return body
		}
		parsed["model"] = json.RawMessage(modelJSON)
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(parsed); err != nil {
			return body
		}
		// json.Encoder.Encode appends a newline; trim it.
		result := buf.Bytes()
		if len(result) > 0 && result[len(result)-1] == '\n' {
			result = result[:len(result)-1]
		}
		return result
	}

	return body
}
