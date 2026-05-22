// forward.go — HTTP 转发、协议认证注入、响应写回与重试逻辑。
package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"time"

	"github.com/ycgame/llms-proxy/internal/auth"
	"github.com/ycgame/llms-proxy/internal/config"
	"github.com/ycgame/llms-proxy/internal/errorlog"
	appmiddleware "github.com/ycgame/llms-proxy/internal/middleware"
	"github.com/ycgame/llms-proxy/internal/usage"
)

const headerAzureAuthorization = "X-Azure-Authorization"

// hopHeaders 使用 Canonical MIME Header 格式，与 http.Header 存储格式一致，
// 避免 copyHeaders 中每个 header 都做 strings.ToLower 分配。
var hopHeaders = map[string]struct{}{
	"Connection":          {},
	"Proxy-Connection":    {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Te":                  {},
	"Trailers":            {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
}

type forwardAttemptError struct {
	status          int
	retryable       bool
	err             error
	startedAt       time.Time // forwardRequest 进入的时刻；用于 errorlog duration 统计
	upstreamURL     string    // 已构造的上游 URL（去 query）；buildURL 后填充，便于错误日志归因
	upstreamFullURL string    // 完整 URL（含 query），用于核验 buildURL 输出（如 azure api-version）
}

func (e *forwardAttemptError) Error() string {
	if e == nil || e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e *forwardAttemptError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func (s *Service) forwardRequest(r *http.Request, state *targetState, body []byte) (*http.Response, context.CancelFunc, int, error) {
	startedAt := time.Now()
	target := state.Target()
	if target == nil {
		return nil, nil, -1, &forwardAttemptError{
			status:    http.StatusBadGateway,
			retryable: false,
			err:       errors.New("target not configured"),
			startedAt: startedAt,
		}
	}

	// 选择 key：如果有 key 池则按客户端亲和选（hash 绑定），否则用 target.APIKey
	apiKey := target.APIKey
	keyIndex := -1
	if state.keyPool != nil {
		clientName := ""
		if principal, ok := auth.PrincipalFromContext(r.Context()); ok && principal != nil {
			clientName = principal.Name
		}
		selected, idx := state.keyPool.selectKeyForClient(clientName)
		if selected == "" {
			return nil, nil, -1, &forwardAttemptError{
				status:    http.StatusServiceUnavailable,
				retryable: false,
				err:       fmt.Errorf("proxy: all API keys exhausted for target %q", target.Name),
				startedAt: startedAt,
			}
		}
		apiKey = selected
		keyIndex = idx
		s.logger.Debug("[keypool] key selected",
			"request_id", appmiddleware.RequestIDFromContext(r.Context()),
			"target", target.Name,
			"client", clientName,
			"key_index", idx,
			"key", maskAPIKey(selected),
		)
	}

	azureAuth := strings.TrimSpace(r.Header.Get(headerAzureAuthorization))
	forwardURL, err := s.buildURL(target, r.URL)
	if err != nil {
		return nil, nil, keyIndex, &forwardAttemptError{
			status:    http.StatusBadGateway,
			retryable: false,
			err:       err,
			startedAt: startedAt,
		}
	}
	fullURL := forwardURL.String()
	upstreamURL := stripURLQuery(fullURL)
	upstreamFullURL := fullURL

	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.getRequestTimeout())

	req, err := http.NewRequestWithContext(ctx, r.Method, fullURL, bodyReader)
	if err != nil {
		cancel()
		return nil, nil, keyIndex, &forwardAttemptError{
			status:          http.StatusBadRequest,
			retryable:       false,
			err:             err,
			startedAt:       startedAt,
			upstreamURL:     upstreamURL,
			upstreamFullURL: upstreamFullURL,
		}
	}

	if len(body) > 0 {
		req.ContentLength = int64(len(body))
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}
	} else if r.Body == nil || r.Body == http.NoBody {
		req.Body = http.NoBody
	}

	copyHeaders(req.Header, r.Header)
	req.Header.Del("Authorization")
	req.Header.Del(headerProxyTarget)
	req.Header.Del(headerAzureAuthorization)
	req.Header.Del("api-key")

	// Inject upstream credentials based on endpoint type.
	switch target.EndpointType {
	case config.EndpointTypeOpenAI:
		req.Header.Set("Authorization", "Bearer "+apiKey)
	case config.EndpointTypeClaude:
		if target.AuthMode == "bearer" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		} else {
			req.Header.Set("x-api-key", apiKey)
		}
		if req.Header.Get("anthropic-version") == "" {
			req.Header.Set("anthropic-version", "2023-06-01")
		}
	case config.EndpointTypeGemini:
		req.Header.Set("x-goog-api-key", apiKey)
	case config.EndpointTypeWangsuOpenAI:
		req.Header.Set("Authorization", "Bearer "+apiKey)
	case config.EndpointTypeWangsuClaude:
		if target.AuthMode == "bearer" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		} else {
			req.Header.Set("x-api-key", apiKey)
		}
		if req.Header.Get("anthropic-version") == "" {
			req.Header.Set("anthropic-version", "2023-06-01")
		}
	case config.EndpointTypeWangsuGemini:
		req.Header.Set("x-goog-api-key", apiKey)
	case config.EndpointTypeWangsuOpenAIImage, config.EndpointTypeWangsuOpenAIImageEdit:
		// 网宿图像通道（文生图 / 图编辑）：Bearer 认证，URL 由 buildURL 整体替换。
		req.Header.Set("Authorization", "Bearer "+apiKey)
	case config.EndpointTypeDeepSeek:
		// DeepSeek 官方：OpenAI 兼容 / Anthropic 兼容两种格式都使用 Bearer 鉴权。
		// 上游路径分流由 buildURL 完成（/v1/messages* 自动加 /anthropic 前缀）。
		req.Header.Set("Authorization", "Bearer "+apiKey)
	case config.EndpointTypeBailian:
		// 百炼 Token Plan：OpenAI 兼容 / Anthropic 兼容两种格式都使用 Bearer 鉴权。
		// 上游路径分流由 buildURL 完成（/v1/messages* 走 /apps/anthropic，其余走 /compatible-mode）。
		req.Header.Set("Authorization", "Bearer "+apiKey)
		if isAnthropicStylePath(r.URL.Path) {
			if req.Header.Get("anthropic-version") == "" {
				req.Header.Set("anthropic-version", "2023-06-01")
			}
		}
	case config.EndpointTypeCopilot:
		// Copilot 动态 token 由 HandleCopilotPassthrough（/copilot/* 路径）处理。
		// 此处仅作为降级路径（copilotService 未配置时使用静态 APIKey）。
		req.Header.Set("Authorization", "Bearer "+apiKey)
	case config.EndpointTypeAzureOpenAI:
		useBearer := target.AllowBearer && azureAuth != ""
		if useBearer {
			req.Header.Set("Authorization", azureAuth)
		} else {
			if apiKey == "" {
				cancel()
				return nil, nil, keyIndex, &forwardAttemptError{
					status:          http.StatusBadRequest,
					retryable:       false,
					err:             errors.New("proxy: missing azure credential; provide api key or X-Azure-Authorization"),
					startedAt:       startedAt,
					upstreamURL:     upstreamURL,
					upstreamFullURL: upstreamFullURL,
				}
			}
			req.Header.Set("api-key", apiKey)
		}
	default:
		cancel()
		return nil, nil, keyIndex, &forwardAttemptError{
			status:          http.StatusInternalServerError,
			retryable:       false,
			err:             fmt.Errorf("proxy: unsupported endpoint type %q for target %q", target.EndpointType, target.Name),
			startedAt:       startedAt,
			upstreamURL:     upstreamURL,
			upstreamFullURL: upstreamFullURL,
		}
	}
	req.Host = target.Endpoint.Host

	// ─── httptrace 埋点：记录连接各阶段耗时，定位大 body 上传卡顿位置 ───
	reqID := appmiddleware.RequestIDFromContext(r.Context())
	reqBodySize := len(body)
	traceStart := time.Now()
	var (
		dnsStart, dnsDone       time.Time
		connStart, connDone     time.Time
		tlsStart, tlsDone       time.Time
		gotConn                 time.Time
		wroteHeaders            time.Time
		wroteRequest            time.Time
		gotFirstResponseByte    time.Time
		connReused              bool
		connRemoteAddr          string
		tlsVersion              string
		httpProto               string
	)
	trace := &httptrace.ClientTrace{
		DNSStart:  func(_ httptrace.DNSStartInfo) { dnsStart = time.Now() },
		DNSDone:   func(_ httptrace.DNSDoneInfo) { dnsDone = time.Now() },
		ConnectStart: func(_, _ string) { connStart = time.Now() },
		ConnectDone:  func(_, _ string, _ error) { connDone = time.Now() },
		TLSHandshakeStart: func() { tlsStart = time.Now() },
		TLSHandshakeDone: func(state tls.ConnectionState, _ error) {
			tlsDone = time.Now()
			switch state.Version {
			case tls.VersionTLS13:
				tlsVersion = "TLS1.3"
			case tls.VersionTLS12:
				tlsVersion = "TLS1.2"
			default:
				tlsVersion = fmt.Sprintf("0x%04x", state.Version)
			}
			httpProto = state.NegotiatedProtocol // "h2" or "http/1.1"
		},
		GotConn: func(info httptrace.GotConnInfo) {
			gotConn = time.Now()
			connReused = info.Reused
			if info.Conn != nil {
				connRemoteAddr = info.Conn.RemoteAddr().String()
			}
		},
		WroteHeaders: func() { wroteHeaders = time.Now() },
		WroteRequest: func(_ httptrace.WroteRequestInfo) { wroteRequest = time.Now() },
		GotFirstResponseByte: func() { gotFirstResponseByte = time.Now() },
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))

	resp, err := s.httpClient.Do(req)

	// 对大请求（>100KB）或慢请求（>5s）输出详细 trace 日志
	totalDuration := time.Since(traceStart)
	if reqBodySize > 100*1024 || totalDuration > 5*time.Second {
		logFields := []any{
			"request_id", reqID,
			"target", targetName(target),
			"method", r.Method,
			"path", r.URL.Path,
			"req_bytes", reqBodySize,
			"total_ms", totalDuration.Milliseconds(),
			"conn_reused", connReused,
			"remote_addr", connRemoteAddr,
			"tls_version", tlsVersion,
			"http_proto", httpProto,
		}
		if !dnsStart.IsZero() && !dnsDone.IsZero() {
			logFields = append(logFields, "dns_ms", dnsDone.Sub(dnsStart).Milliseconds())
		}
		if !connStart.IsZero() && !connDone.IsZero() {
			logFields = append(logFields, "tcp_connect_ms", connDone.Sub(connStart).Milliseconds())
		}
		if !tlsStart.IsZero() && !tlsDone.IsZero() {
			logFields = append(logFields, "tls_handshake_ms", tlsDone.Sub(tlsStart).Milliseconds())
		}
		if !gotConn.IsZero() {
			logFields = append(logFields, "get_conn_ms", gotConn.Sub(traceStart).Milliseconds())
		}
		if !wroteHeaders.IsZero() && !gotConn.IsZero() {
			logFields = append(logFields, "write_headers_ms", wroteHeaders.Sub(gotConn).Milliseconds())
		}
		if !wroteRequest.IsZero() && !wroteHeaders.IsZero() {
			logFields = append(logFields, "write_body_ms", wroteRequest.Sub(wroteHeaders).Milliseconds())
		}
		if !gotFirstResponseByte.IsZero() && !wroteRequest.IsZero() {
			logFields = append(logFields, "wait_response_ms", gotFirstResponseByte.Sub(wroteRequest).Milliseconds())
		}
		if resp != nil {
			logFields = append(logFields, "status", resp.StatusCode)
		}
		if err != nil {
			logFields = append(logFields, "error", err.Error())
		}
		s.logger.Warn("upstream request trace", logFields...)
	}

	if err != nil {
		cancel()
		retryable := !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled)
		return nil, nil, keyIndex, &forwardAttemptError{
			status:          classifyTransportError(err),
			retryable:       retryable,
			err:             err,
			startedAt:       startedAt,
			upstreamURL:     upstreamURL,
			upstreamFullURL: upstreamFullURL,
		}
	}

	// key 池耗尽检测：检查上游响应是否表示 key 额度耗尽
	if state.keyPool != nil && keyIndex >= 0 && resp != nil && resp.StatusCode >= 400 {
		checkBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(checkBody))

		if exhausted, code := isKeyExhausted(resp.StatusCode, checkBody); exhausted {
			state.keyPool.markExhausted(keyIndex, code)
		}
	}

	return resp, cancel, keyIndex, nil
}

func (s *Service) writeResponse(
	w http.ResponseWriter,
	r *http.Request,
	state *targetState,
	resp *http.Response,
	cancel context.CancelFunc,
	attempt int,
	model string,
	requestBody []byte,
	startedAt time.Time,
) {
	defer deferCancel(cancel)
	defer resp.Body.Close()

	target := state.Target()

	// SSE 自动聚合：如果客户端请求 non-streaming 但上游返回了 SSE，聚合为标准 JSON
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		respCT := resp.Header.Get("Content-Type")
		if shouldAggregateSSE(target, requestBody, respCT) {
			sseBody, err := io.ReadAll(resp.Body)
			if err == nil {
				aggregated, newCT, aggErr := aggregateSSEResponse(sseBody)
				if aggErr == nil {
					s.logger.Info("aggregated SSE response to non-streaming JSON",
						"request_id", appmiddleware.RequestIDFromContext(r.Context()),
						"target", targetName(target),
						"original_size", len(sseBody),
						"aggregated_size", len(aggregated),
					)
					// 替换响应头和body
					for key, values := range resp.Header {
						if _, skip := hopHeaders[key]; skip {
							continue
						}
						for _, v := range values {
							w.Header().Add(key, v)
						}
					}
					w.Header().Set("Content-Type", newCT)
					if target != nil {
						w.Header().Set("X-Proxy-Target", target.Name)
						w.Header().Set("X-Azure-Target", target.Name)
					}
					w.Header().Set("X-SSE-Aggregated", "true")
					w.WriteHeader(resp.StatusCode)
					w.Write(aggregated)

					state.MarkSuccess(time.Now())
					s.metrics.totalSuccess.Add(1)
					// record usage from aggregated body
					s.recordUsageEvent(r, target, resp.StatusCode, model, newCT, aggregated)
					return
				}
				s.logger.Warn("SSE aggregation failed, falling back to raw response",
					"request_id", appmiddleware.RequestIDFromContext(r.Context()),
					"target", targetName(target),
					"error", aggErr,
				)
				// 聚合失败，用原始 SSE body 继续走正常流程
				resp.Body = io.NopCloser(bytes.NewReader(sseBody))
			}
		}
	}

	for key, values := range resp.Header {
		if _, skip := hopHeaders[key]; skip {
			continue
		}
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
	if target != nil {
		w.Header().Set("X-Proxy-Target", target.Name)
		w.Header().Set("X-Azure-Target", target.Name) // backward compat
	}
	w.WriteHeader(resp.StatusCode)

	if isUpstreamFailureStatus(resp.StatusCode) {
		state.MarkFailure(time.Now(), s.quietPeriod)
		stats := state.Stats()
		s.metrics.totalFailures.Add(1)
		s.logger.Warn("target response error",
			"request_id", appmiddleware.RequestIDFromContext(r.Context()),
			"target", targetName(target),
			"status_code", resp.StatusCode,
			"consecutive_failures", stats.ConsecutiveFailure,
			"total_failures", stats.TotalFailure,
			"muted_until", stats.MutedUntil,
		)
	} else {
		state.MarkSuccess(time.Now())
		s.metrics.totalSuccess.Add(1)
		if attempt > 1 {
			s.logger.Info("request succeeded after failover",
				"request_id", appmiddleware.RequestIDFromContext(r.Context()),
				"target", targetName(target),
				"attempt", attempt,
				"status_code", resp.StatusCode,
			)
		}
	}

	writer := newStreamingWriter(w)
	capture := &limitedCaptureWriter{limit: 2 * 1024 * 1024}
	if _, err := io.Copy(writer, io.TeeReader(resp.Body, capture)); err != nil &&
		!errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded) {
		s.logger.Warn("stream copy failed",
			"request_id", appmiddleware.RequestIDFromContext(r.Context()),
			"target", targetName(target),
			"error", err,
		)
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		s.recordUsageEvent(r, target, resp.StatusCode, model, resp.Header.Get("Content-Type"), capture.Bytes())
	}

	// 上游 4xx/5xx：写结构化错误日志（旁路 access/error log，便于事后 grep）。
	// 触发条件覆盖 4xx 与 5xx 完整范围；用户主动 abort 由 io.Copy err 分支拦截不入此路径。
	if resp.StatusCode >= 400 {
		writeUpstreamErrorLog(r, target, resp, capture.Bytes(), startedAt)
	}
}

// writeUpstreamErrorLog 在 writeResponse 写完响应体后落一条 NDJSON 错误日志。
// resp_excerpt 取 capture（已限 2MB）的前 1024 字节。
func writeUpstreamErrorLog(r *http.Request, target *Target, resp *http.Response, body []byte, startedAt time.Time) {
	kind := errorlog.KindUpstream4xx
	if resp.StatusCode >= 500 {
		kind = errorlog.KindUpstream5xx
	}

	excerpt := body
	if len(excerpt) > 1024 {
		excerpt = excerpt[:1024]
	}

	entry := errorlog.Entry{
		TraceID:        appmiddleware.RequestIDFromContext(r.Context()),
		Kind:           kind,
		Method:         r.Method,
		Path:           r.URL.Path,
		ClientIP:       clientIP(r),
		UpstreamStatus: resp.StatusCode,
		DurationMS:     time.Since(startedAt).Milliseconds(),
		ReqBytes:       int(r.ContentLength),
		RespBytes:      len(body),
		RespExcerpt:    string(excerpt),
	}
	if target != nil {
		entry.Target = target.Name
		entry.EndpointType = target.EndpointType
		if target.Endpoint != nil {
			entry.UpstreamURL = stripURLQuery(target.Endpoint.String())
		}
	}
	// 完整 URL（含 query）从实际发出的请求取，便于核验 buildURL 输出（如 azure api-version）。
	if resp != nil && resp.Request != nil && resp.Request.URL != nil {
		entry.UpstreamFullURL = resp.Request.URL.String()
	}
	errorlog.Write(entry)
}

func (s *Service) recordUsageEvent(r *http.Request, target *Target, statusCode int, model string, contentType string, body []byte) {
	recorder := s.currentUsageRecorder()
	if recorder == nil || r == nil {
		return
	}

	principal, ok := auth.PrincipalFromContext(r.Context())
	if !ok || principal == nil {
		return
	}

	tokens, parsedModel, found := extractUsageTokens(contentType, body)
	if !found {
		return
	}

	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		model = parsedModel
	}

	var endpointType string
	if target != nil {
		endpointType = target.EndpointType
	}

	// 规范化模型名：把客户端传入的 alias（如 deepseek-chat）解析为 catalog 中的
	// 规范名（如 deepseek-v4-flash），让用量统计、价格匹配全链路按规范名聚合。
	// 找不到 catalog 或 model 不是别名时，ResolveAlias 原样返回。
	if cat := getLocalCatalog(); cat != nil && model != "" {
		model = cat.ResolveAlias(endpointType, model)
	}

	evt := usage.Event{
		Timestamp:    time.Now().UTC(),
		ClientName:   principal.Name,
		EndpointType: endpointType,
		Model:        model,
		InputTokens:  tokens.InputTokens,
		OutputTokens: tokens.OutputTokens,
		CachedTokens: tokens.CachedTokens,
		RequestID:    appmiddleware.RequestIDFromContext(r.Context()),
		StatusCode:   statusCode,
		Path:         r.URL.Path,
	}
	if target != nil {
		evt.Target = target.Name
	}

	if err := recorder.Record(evt); err != nil {
		s.logger.Warn("failed to record usage event",
			"request_id", appmiddleware.RequestIDFromContext(r.Context()),
			"client", principal.Name,
			"error", err,
		)
	}
}

func (s *Service) handleForwardError(r *http.Request, state *targetState, err error, status int) {
	if status == 0 {
		status = http.StatusBadGateway
	}
	state.MarkFailure(time.Now(), s.quietPeriod)
	stats := state.Stats()

	s.logger.Warn("forward request failed",
		"request_id", appmiddleware.RequestIDFromContext(r.Context()),
		"target", state.Target().Name,
		"error", err,
		"status", status,
		"consecutive_failures", stats.ConsecutiveFailure,
		"total_failures", stats.TotalFailure,
		"muted_until", stats.MutedUntil,
	)
}

// writeForwardErrorLog 在 forwardRequest 失败（网络错误 / 配置错误 / Azure 缺凭据 等）时落错误日志。
// 由 service.go 在拿到 forwardAttemptError 后统一调用，传入完整 startedAt / upstreamURL。
func writeForwardErrorLog(r *http.Request, state *targetState, fe *forwardAttemptError) {
	if fe == nil {
		return
	}
	target := state.Target()

	entry := errorlog.Entry{
		TraceID:         appmiddleware.RequestIDFromContext(r.Context()),
		Kind:            errorlog.KindUpstreamNetErr,
		Method:          r.Method,
		Path:            r.URL.Path,
		ClientIP:        clientIP(r),
		UpstreamStatus:  0, // net error 时上游无响应
		UpstreamURL:     fe.upstreamURL,
		UpstreamFullURL: fe.upstreamFullURL,
		ReqBytes:        int(r.ContentLength),
	}
	if !fe.startedAt.IsZero() {
		entry.DurationMS = time.Since(fe.startedAt).Milliseconds()
	}
	if fe.err != nil {
		entry.Error = fe.err.Error()
	}
	if target != nil {
		entry.Target = target.Name
		entry.EndpointType = target.EndpointType
	}
	errorlog.Write(entry)
}

// clientIP 提取请求来源 IP，与 access log 的 remote_ip 字段语义一致。
func clientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if comma := strings.Index(xff, ","); comma > 0 {
			return strings.TrimSpace(xff[:comma])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// stripURLQuery 去掉 URL 中的 query string，避免 errorlog 泄露 api key 等查询参数。
func stripURLQuery(raw string) string {
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		return raw[:i]
	}
	return raw
}

func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		if _, skip := hopHeaders[key]; skip {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

type streamingWriter struct {
	http.ResponseWriter
	flusher http.Flusher
}

func newStreamingWriter(w http.ResponseWriter) *streamingWriter {
	sw := &streamingWriter{ResponseWriter: w}
	if fl, ok := w.(http.Flusher); ok {
		sw.flusher = fl
	}
	return sw
}

func (w *streamingWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if err == nil && w.flusher != nil {
		w.flusher.Flush()
	}
	return n, err
}

func classifyTransportError(err error) int {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return http.StatusGatewayTimeout
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return http.StatusGatewayTimeout
	}

	return http.StatusBadGateway
}

func targetName(t *Target) string {
	if t == nil {
		return ""
	}
	return t.Name
}

// isUpstreamFailureStatus 判定上游响应状态码是否应触发 MarkFailure（进而 mute + 下次请求 fallback）。
//
// 触发条件：
//   - 5xx：上游服务端错误（502/503/504 等），通用故障切换的信号；
//   - 429：上游过载/限流（OpenAI/Azure 在模型容量瓶颈时常返回此码，含 "Engine is overloaded"）；
//   - 408：上游请求超时（典型见于网宿/Azure 长耗时图像生成接口）。
//
// 4xx 其他状态（400/401/403/404 等）属于客户端请求问题，不切换 target。
func isUpstreamFailureStatus(statusCode int) bool {
	if statusCode >= 500 {
		return true
	}
	switch statusCode {
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	}
	return false
}

// isKeyExhausted 检查上游响应是否表示 key 额度耗尽（适用于所有 provider）。
// 仅检查非 2xx 响应。纯 429 限流（RPS/RPM）不算耗尽，由 failover 逻辑处理。
// 错误码来源：各 provider 官方文档。
// 返回 (是否耗尽, 错误码)。
func isKeyExhausted(statusCode int, body []byte) (bool, string) {
	if statusCode >= 200 && statusCode < 300 {
		return false, ""
	}
	bodyStr := string(body)
	lower := strings.ToLower(bodyStr)

	// ── 401: key/token 无效或过期 ──
	// OpenAI: "Incorrect API key provided"
	// Azure:  "Access denied due to invalid subscription key"
	// Claude: "invalid x-api-key" (authentication_error)
	// 百炼:   "Invalid API-key provided" / "Incorrect API key provided" (InvalidApiKey)
	if statusCode == 401 {
		if strings.Contains(lower, "invalid api key") ||
			strings.Contains(lower, "invalid api-key") ||
			strings.Contains(lower, "invalid_api_key") || // DeepSeek: code="invalid_api_key"
			strings.Contains(lower, "invalid access token") ||
			strings.Contains(lower, "invalid x-api-key") ||
			strings.Contains(lower, "token expired") ||
			strings.Contains(lower, "access denied due to invalid subscription key") ||
			strings.Contains(lower, "incorrect api key") {
			return true, "invalid_token"
		}
	}

	// ── 400: API key 格式错误 ──
	// Gemini: "API key not valid" (INVALID_ARGUMENT)
	if statusCode == 400 {
		if strings.Contains(bodyStr, "API_KEY_INVALID") ||
			strings.Contains(bodyStr, "API key not valid") {
			return true, "invalid_token"
		}
	}

	// ── 402: 账单/余额问题 ──
	// DeepSeek: "Insufficient Balance"
	// Claude:   billing_error
	if statusCode == 402 {
		return true, "billing_error"
	}

	// ── 百炼: 账户欠费（HTTP 400）──
	// 官方文档: code=Arrearage, "Access denied, please make sure your account is in good standing."
	if strings.Contains(bodyStr, `"Arrearage"`) ||
		strings.Contains(bodyStr, `"code":"Arrearage"`) ||
		strings.Contains(bodyStr, `"code": "Arrearage"`) {
		return true, "Arrearage"
	}

	// ── 百炼: 免费额度用完 / 未开通（HTTP 403）──
	// 官方文档: code=AccessDenied.Unpurchased / AllocationQuota.FreeTierOnly
	if strings.Contains(bodyStr, "AccessDenied.Unpurchased") {
		return true, "AccessDenied.Unpurchased"
	}
	if strings.Contains(bodyStr, "AllocationQuota.FreeTierOnly") {
		return true, "free_tier_exhausted"
	}

	// ── 配额耗尽（必须在 429 限流排除之前检测）──
	// OpenAI:  "You exceeded your current quota" (429)
	// Gemini:  "RESOURCE_EXHAUSTED" (429)
	// 百炼:    code=QuotaExceeded / Throttling.AllocationQuota (429, TPS/TPM 配额耗尽)
	// 百炼:    PrepaidBillOverdue / PostpaidBillOverdue / CommodityNotPurchased (429, 账单过期)
	if strings.Contains(lower, "quota exceeded") ||
		strings.Contains(lower, "exceeded your quota") ||
		strings.Contains(lower, "you exceeded your current quota") ||
		strings.Contains(lower, "resource_exhausted") ||
		strings.Contains(bodyStr, `"code":"QuotaExceeded"`) ||
		strings.Contains(bodyStr, "Throttling.AllocationQuota") ||
		strings.Contains(bodyStr, "PrepaidBillOverdue") ||
		strings.Contains(bodyStr, "PostpaidBillOverdue") ||
		strings.Contains(bodyStr, "CommodityNotPurchased") {
		return true, "quota_exceeded"
	}

	// ── 账户被禁用/封禁 ──
	if strings.Contains(lower, "account disabled") ||
		strings.Contains(lower, "account suspended") ||
		strings.Contains(lower, "account has been deactivated") {
		return true, "account_disabled"
	}

	// ── 纯 429 限流排除（RPS/RPM 级别，不算 key 耗尽）──
	// 百炼:    Throttling / Throttling.RateQuota / Throttling.BurstRate
	// OpenAI:  "Rate limit reached for requests"
	// Claude:  rate_limit_error
	// Gemini:  "RESOURCE_EXHAUSTED"（已在上面配额耗尽中匹配）
	// DeepSeek: "Rate Limit Reached"
	if statusCode == 429 {
		if strings.Contains(lower, "throttling") ||
			strings.Contains(lower, "rate limit") ||
			strings.Contains(lower, "too many requests") ||
			strings.Contains(lower, "rate_limit_exceeded") ||
			strings.Contains(lower, "rate_limit_error") {
			return false, ""
		}
	}

	return false, ""
}
