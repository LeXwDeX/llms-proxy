// forward.go — HTTP 转发、协议认证注入、响应写回与重试逻辑。
package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ycgame/llms-proxy/internal/auth"
	"github.com/ycgame/llms-proxy/internal/config"
	"github.com/ycgame/llms-proxy/internal/errorlog"
	appmiddleware "github.com/ycgame/llms-proxy/internal/middleware"
	"github.com/ycgame/llms-proxy/internal/tracestore"
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

// resolveAuthStrategy 返回给定 target 和请求的认证策略。
// 对于大多数 provider，直接返回注册表中的静态策略；
// 对于 Azure 和 Claude，根据 target/request 动态创建（因为 AllowBearer / AuthMode 是 per-target 的）。
func (s *Service) resolveAuthStrategy(target *Target, r *http.Request) AuthStrategy {
	profile := s.providerRegistry.Lookup(target.EndpointType)
	if profile == nil {
		return nil
	}

	switch target.EndpointType {
	case config.EndpointTypeAzureOpenAI:
		azureAuth := strings.TrimSpace(r.Header.Get(headerAzureAuthorization))
		return &AzureAuth{
			AllowBearer:    target.AllowBearer,
			AzureAuthValue: azureAuth,
		}
	case config.EndpointTypeClaude:
		return &AnthropicAuth{
			BearerMode: target.AuthMode == "bearer",
		}
	default:
		return profile.Auth
	}
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

	// 选择 key：如果有 key 池则按客户端亲和选（轮询分配 + 记忆绑定），否则用 target.APIKey
	apiKey := target.APIKey
	keyIndex := -1
	if state.keyPool != nil {
		clientKey := ""
		if principal, ok := auth.PrincipalFromContext(r.Context()); ok && principal != nil {
			clientKey = principal.AccessKey
		}
		// 亲和键：IP + 客户端 access_key，区分同 token 不同机器/用户
		affinityID := clientIP(r) + "|" + clientKey
		selected, idx := state.keyPool.selectKeyForClient(affinityID)
		if selected == "" {
			// 所有 key 不可用，尝试触发唤醒模型
			if state.keyPool.tryWakeUp(time.Now()) {
				s.logger.Warn("[keypool] all keys exhausted, triggering wake-up model",
					"request_id", appmiddleware.RequestIDFromContext(r.Context()),
					"target", target.Name,
				)
				// 执行唤醒探测
				recoveredIdx := s.wakeUpProbe(r, state)
				state.keyPool.wakeUpComplete()
				if recoveredIdx >= 0 {
					// 唤醒成功，重新选择 key
					selected, idx = state.keyPool.selectKeyForClient(affinityID)
					if selected != "" {
						s.logger.Info("[keypool] wake-up successful, key recovered",
							"request_id", appmiddleware.RequestIDFromContext(r.Context()),
							"target", target.Name,
							"key_index", idx,
						)
						apiKey = selected
						keyIndex = idx
						goto keySelected
					}
				}
			}
			return nil, nil, -1, &forwardAttemptError{
				status:    http.StatusServiceUnavailable,
				retryable: false,
				err:       fmt.Errorf("proxy: all API keys exhausted for target %q", target.Name),
				startedAt: startedAt,
			}
		}
		apiKey = selected
		keyIndex = idx
	keySelected:
		clientName := ""
		if principal, ok := auth.PrincipalFromContext(r.Context()); ok && principal != nil {
			clientName = principal.Name
		}
		s.logger.Debug("[keypool] key selected",
			"request_id", appmiddleware.RequestIDFromContext(r.Context()),
			"target", target.Name,
			"client", clientName,
			"client_ip", clientIP(r),
			"key_index", idx,
			"key", maskAPIKey(selected),
		)
	}

	// #3 在途计数：选中 key 后登记该 key 的并发请求，转发结束时释放。
	// 释放保证：要么经由返回的 resp.Body.Close()（成功路径），要么经由下方 defer
	// 在出错返回（无可用 resp）时兜底，二者由 sync.Once 去重，确保不漏放也不重复放。
	var inflightRelease func()
	if state.keyPool != nil && keyIndex >= 0 {
		kp := state.keyPool
		ki := keyIndex
		kp.acquireInFlight(ki)
		var once sync.Once
		inflightRelease = func() { once.Do(func() { kp.releaseInFlight(ki) }) }
	}
	inflightHandedOff := false
	defer func() {
		if inflightRelease != nil && !inflightHandedOff {
			inflightRelease()
		}
	}()

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

	// Inject upstream credentials based on provider profile.
	authStrategy := s.resolveAuthStrategy(target, r)
	if authStrategy == nil {
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

	// Azure 特殊处理：空 apiKey 且非 Bearer 模式时返回错误
	if target.EndpointType == config.EndpointTypeAzureOpenAI {
		if !target.AllowBearer || azureAuth == "" {
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
		}
	}

	if err := authStrategy.InjectAuth(req, apiKey, r.URL.Path); err != nil {
		cancel()
		return nil, nil, keyIndex, &forwardAttemptError{
			status:          http.StatusInternalServerError,
			retryable:       false,
			err:             fmt.Errorf("proxy: auth injection failed for target %q: %w", target.Name, err),
			startedAt:       startedAt,
			upstreamURL:     upstreamURL,
			upstreamFullURL: upstreamFullURL,
		}
	}
	req.Host = target.Endpoint.Host

	// ─── httptrace 埋点：仅对大请求（>100KB）启用，避免每个请求分配 10 个闭包 ───
	reqID := appmiddleware.RequestIDFromContext(r.Context())
	reqBodySize := len(body)
	traceStart := time.Now()
	traceEnabled := reqBodySize > 100*1024
	var (
		dnsStart, dnsDone    time.Time
		connStart, connDone  time.Time
		tlsStart, tlsDone    time.Time
		gotConn              time.Time
		wroteHeaders         time.Time
		wroteRequest         time.Time
		gotFirstResponseByte time.Time
		connReused           bool
		connRemoteAddr       string
		tlsVersion           string
		httpProto            string
	)
	if traceEnabled {
		trace := &httptrace.ClientTrace{
			DNSStart:          func(_ httptrace.DNSStartInfo) { dnsStart = time.Now() },
			DNSDone:           func(_ httptrace.DNSDoneInfo) { dnsDone = time.Now() },
			ConnectStart:      func(_, _ string) { connStart = time.Now() },
			ConnectDone:       func(_, _ string, _ error) { connDone = time.Now() },
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
			WroteHeaders:         func() { wroteHeaders = time.Now() },
			WroteRequest:         func(_ httptrace.WroteRequestInfo) { wroteRequest = time.Now() },
			GotFirstResponseByte: func() { gotFirstResponseByte = time.Now() },
		}
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	}

	resp, err := s.httpClient.Do(req)

	// 提取上游响应头中的关键标识，用于问题排查时关联百炼等上游的 request_id
	var upstreamRequestID string
	if resp != nil {
		upstreamRequestID = resp.Header.Get("X-Request-Id")
	}

	// 对大请求（>100KB）输出详细 trace 日志；对慢请求（>5s）输出简要日志
	totalDuration := time.Since(traceStart)
	if traceEnabled {
		logFields := []any{
			"request_id", reqID,
			"upstream_request_id", upstreamRequestID,
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
	} else if totalDuration > 5*time.Second {
		s.logger.Warn("upstream slow request",
			"request_id", reqID,
			"upstream_request_id", upstreamRequestID,
			"target", targetName(target),
			"method", r.Method,
			"path", r.URL.Path,
			"req_bytes", reqBodySize,
			"total_ms", totalDuration.Milliseconds(),
		)
	}

	// 每个请求都记录 proxy → upstream request_id 映射，便于向上游厂商报障
	if upstreamRequestID != "" {
		s.logger.Info("upstream request id mapping",
			"request_id", reqID,
			"upstream_request_id", upstreamRequestID,
			"target", targetName(target),
			"path", r.URL.Path,
			"total_ms", totalDuration.Milliseconds(),
		)
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

		// 如果响应体是 gzip 压缩的，先解压再做模式匹配
		exhaustionBody := checkBody
		if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
			if gr, err := gzip.NewReader(bytes.NewReader(checkBody)); err == nil {
				if decompressed, err := io.ReadAll(io.LimitReader(gr, 8192)); err == nil {
					exhaustionBody = decompressed
				}
				gr.Close()
			}
		}

		if exhausted, code := isKeyExhausted(resp.StatusCode, exhaustionBody); exhausted {
			state.keyPool.markExhaustedWithError(keyIndex, code, string(exhaustionBody))
		}
	}

	// #3 将在途计数的释放移交给响应体：调用方关闭 resp.Body（重试循环或 writeResponse
	// 的 defer）时释放，覆盖流式响应的完整生命周期。
	if inflightRelease != nil && resp != nil {
		resp.Body = &releaseOnCloseBody{ReadCloser: resp.Body, release: inflightRelease}
		inflightHandedOff = true
	}

	return resp, cancel, keyIndex, nil
}

// releaseOnCloseBody 包裹上游响应体，在 Close 时触发一次性的资源释放回调
// （用于 #3 在途计数释放），随后关闭底层响应体。
type releaseOnCloseBody struct {
	io.ReadCloser
	release func()
}

func (b *releaseOnCloseBody) Close() error {
	if b.release != nil {
		b.release()
	}
	return b.ReadCloser.Close()
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
	keyIndex int,
) {
	defer deferCancel(cancel)
	defer resp.Body.Close()

	// 提取上游 request_id，用于回传给客户端做问题排查关联
	upstreamRequestID := resp.Header.Get("X-Request-Id")

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
					if upstreamRequestID != "" {
						w.Header().Set("X-Upstream-Request-Id", upstreamRequestID)
					}
					w.WriteHeader(resp.StatusCode)
					w.Write(aggregated)

					state.MarkSuccess(time.Now())
					s.metrics.totalSuccess.Add(1)
					// record usage from aggregated body
					s.recordUsageEvent(r, target, resp.StatusCode, model, newCT, aggregated, state, keyIndex)
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
	if upstreamRequestID != "" {
		w.Header().Set("X-Upstream-Request-Id", upstreamRequestID)
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
		s.recordUsageEvent(r, target, resp.StatusCode, model, resp.Header.Get("Content-Type"), capture.Bytes(), state, keyIndex)
	}

	// 上游 4xx/5xx：写结构化错误日志（旁路 access/error log，便于事后 grep）。
	// 触发条件覆盖 4xx 与 5xx 完整范围；用户主动 abort 由 io.Copy err 分支拦截不入此路径。
	if resp.StatusCode >= 400 {
		writeUpstreamErrorLog(r, target, resp, capture.Bytes(), startedAt)
	}

	// DEBUG 模式：记录完整 trace（请求/响应 META + body）
	if s.traceStore != nil {
		s.recordTrace(r, target, resp, requestBody, capture.Bytes(), startedAt, keyIndex, model)
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

	// 如果响应体是 gzip 压缩的，解压后再取 excerpt（避免日志中出现乱码）
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		if gr, err := gzip.NewReader(bytes.NewReader(body)); err == nil {
			if decompressed, err := io.ReadAll(io.LimitReader(gr, 2048)); err == nil {
				excerpt = decompressed
				if len(excerpt) > 1024 {
					excerpt = excerpt[:1024]
				}
			}
			gr.Close()
		}
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

// recordTrace 记录完整的请求/响应 trace（DEBUG 模式）。
func (s *Service) recordTrace(r *http.Request, target *Target, resp *http.Response, requestBody, responseBody []byte, startedAt time.Time, keyIndex int, model string) {
	if s.traceStore == nil {
		return
	}

	principal, _ := auth.PrincipalFromContext(r.Context())
	clientName := ""
	clientAccessKey := ""
	if principal != nil {
		clientName = principal.Name
		clientAccessKey = maskAPIKey(principal.AccessKey)
	}

	// 收集请求头（脱敏）
	requestHeaders := make(map[string]string)
	for key := range r.Header {
		lower := strings.ToLower(key)
		if lower == "authorization" || lower == "x-api-key" || lower == "x-goog-api-key" || lower == "cookie" {
			requestHeaders[key] = "***"
		} else {
			requestHeaders[key] = r.Header.Get(key)
		}
	}

	// 收集上游响应头（脱敏）
	upstreamHeaders := make(map[string]string)
	if resp != nil {
		for key := range resp.Header {
			lower := strings.ToLower(key)
			if lower == "authorization" || lower == "x-api-key" || lower == "x-goog-api-key" || lower == "set-cookie" {
				upstreamHeaders[key] = "***"
			} else {
				upstreamHeaders[key] = resp.Header.Get(key)
			}
		}
	}

	// 构建上游 URL（脱敏 query 中的 api-key 参数）
	upstreamURL := ""
	if resp != nil && resp.Request != nil && resp.Request.URL != nil {
		u := *resp.Request.URL
		q := u.Query()
		for param := range q {
			lower := strings.ToLower(param)
			if lower == "api-key" || lower == "apikey" || lower == "key" || lower == "api_key" {
				q.Set(param, "***")
			}
		}
		u.RawQuery = q.Encode()
		upstreamURL = u.String()
	}

	// 提取 key mask
	keyMask := ""
	if target != nil && keyIndex >= 0 {
		keys := target.APIKeys
		if len(keys) == 0 && target.APIKey != "" {
			keys = []string{target.APIKey}
		}
		if keyIndex < len(keys) {
			keyMask = maskAPIKey(keys[keyIndex])
		}
	}

	// 如果响应体是 gzip 压缩的，解压后再存储（避免存储乱码）
	respBodyStr := string(responseBody)
	if resp != nil && strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") && len(responseBody) > 0 {
		if gr, err := gzip.NewReader(bytes.NewReader(responseBody)); err == nil {
			if decompressed, err := io.ReadAll(io.LimitReader(gr, int64(s.traceStore.MaxBodySize()))); err == nil {
				respBodyStr = string(decompressed)
			}
			gr.Close()
		}
	}

	record := &tracestore.TraceRecord{
		// META: 请求侧
		TraceID:         appmiddleware.RequestIDFromContext(r.Context()),
		Timestamp:       startedAt,
		ClientName:      clientName,
		ClientIP:        clientIP(r),
		ClientAccessKey: clientAccessKey,
		Method:          r.Method,
		Path:            r.URL.Path,
		QueryParams:     r.URL.RawQuery,
		RequestHeaders:  requestHeaders,

		// META: 路由决策
		Target:       targetName(target),
		EndpointType: targetEndpointType(target),
		Model:        model,
		KeyIndex:     keyIndex,
		KeyMask:      keyMask,

		// META: 上游
		UpstreamURL:       upstreamURL,
		UpstreamRequestID: resp.Header.Get("X-Request-Id"),
		UpstreamStatus:    resp.StatusCode,
		UpstreamHeaders:   upstreamHeaders,

		// META: 结果
		StatusCode: resp.StatusCode,
		DurationMS: time.Since(startedAt).Milliseconds(),

		// 内容
		RequestBody:  string(requestBody),
		ResponseBody: respBodyStr,
	}

	s.traceStore.Record(record)
}

// targetEndpointType 返回 target 的 endpoint type。
func targetEndpointType(target *Target) string {
	if target == nil {
		return ""
	}
	return target.EndpointType
}

func (s *Service) recordUsageEvent(r *http.Request, target *Target, statusCode int, model string, contentType string, body []byte, state *targetState, keyIndex int) {
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

	// 回写 token 用量到 keyPool，用于 token 感知调度
	if state != nil && state.keyPool != nil && keyIndex >= 0 {
		totalTokens := tokens.InputTokens + tokens.OutputTokens
		state.keyPool.recordTokens(keyIndex, totalTokens)
	}

	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		model = parsedModel
	}

	var endpointType string
	if target != nil {
		endpointType = target.EndpointType
	}

	// 规范化模型名：把客户端传入的 alias 解析为 catalog 中的规范名，
	// 让用量统计、价格匹配全链路按规范名聚合。
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

// isKeyExhausted 检查上游响应是否表示 key 真正不可用（适用于所有 provider）。
// 仅检查非 2xx 响应。
//
// 核心原则：只有 key 本身出了问题（无效、欠费、封禁、总额度耗尽）才标记耗尽。
// 429 限流（TPM/RPM）是临时性的，key 本身没问题，不标记——换 key 也没用（同账号共享配额）。
//
// 返回 (是否耗尽, 错误码)。
func isKeyExhausted(statusCode int, body []byte) (bool, string) {
	if statusCode >= 200 && statusCode < 300 {
		return false, ""
	}
	// Use bytes operations to avoid double allocation from string(body) + strings.ToLower.
	// lower is used for case-insensitive matching; body is used for exact-case matching.
	lower := bytes.ToLower(body)

	// ── 401: key/token 无效或过期 ──
	// OpenAI: "Incorrect API key provided"
	// Azure:  "Access denied due to invalid subscription key"
	// Claude: "invalid x-api-key" (authentication_error)
	// 百炼:   "Invalid API-key provided" / "Incorrect API key provided" (InvalidApiKey)
	if statusCode == 401 {
		if bytes.Contains(lower, []byte("invalid api key")) ||
			bytes.Contains(lower, []byte("invalid api-key")) ||
			bytes.Contains(lower, []byte("invalid_api_key")) || // DeepSeek: code="invalid_api_key"
			bytes.Contains(lower, []byte("invalid access token")) ||
			bytes.Contains(lower, []byte("invalid x-api-key")) ||
			bytes.Contains(lower, []byte("token expired")) ||
			bytes.Contains(lower, []byte("access denied due to invalid subscription key")) ||
			bytes.Contains(lower, []byte("incorrect api key")) {
			return true, "invalid_token"
		}
	}

	// ── 400: API key 格式错误 ──
	// Gemini: "API key not valid" (INVALID_ARGUMENT)
	if statusCode == 400 {
		if bytes.Contains(body, []byte("API_KEY_INVALID")) ||
			bytes.Contains(body, []byte("API key not valid")) {
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
	if bytes.Contains(body, []byte(`"Arrearage"`)) ||
		bytes.Contains(body, []byte(`"code":"Arrearage"`)) ||
		bytes.Contains(body, []byte(`"code": "Arrearage"`)) {
		return true, "Arrearage"
	}

	// ── 百炼: 免费额度用完 / 未开通（HTTP 403）──
	// 官方文档: code=AccessDenied.Unpurchased / AllocationQuota.FreeTierOnly
	if bytes.Contains(body, []byte("AccessDenied.Unpurchased")) {
		return true, "AccessDenied.Unpurchased"
	}
	if bytes.Contains(body, []byte("AllocationQuota.FreeTierOnly")) {
		return true, "free_tier_exhausted"
	}

	// ── 真正的配额耗尽（key 的总额度用完了，不是临时限流）──
	// OpenAI:  "You exceeded your current quota" (429)
	// Gemini:  "RESOURCE_EXHAUSTED" (429)
	// 百炼:    code=QuotaExceeded (429, 真正的配额耗尽)
	// 百炼:    PrepaidBillOverdue / PostpaidBillOverdue / CommodityNotPurchased (429, 账单过期)
	// 百炼:    "Your token-plan quota has been exhausted" (429, TokenPlan 总额度耗尽)
	//
	// ⚠️ 不包含：百炼 "Allocated quota exceeded" / insufficient_quota /
	// Throttling.AllocationQuota / "upgrade your API plan" — 这些是 TPM 限流。
	if bytes.Contains(lower, []byte("exceeded your quota")) ||
		bytes.Contains(lower, []byte("you exceeded your current quota")) ||
		bytes.Contains(lower, []byte("resource_exhausted")) ||
		bytes.Contains(lower, []byte("quota has been exhausted")) ||
		bytes.Contains(lower, []byte("token-plan quota")) ||
		bytes.Contains(body, []byte(`"code":"QuotaExceeded"`)) ||
		bytes.Contains(body, []byte("PrepaidBillOverdue")) ||
		bytes.Contains(body, []byte("PostpaidBillOverdue")) ||
		bytes.Contains(body, []byte("CommodityNotPurchased")) {
		return true, "quota_exceeded"
	}

	// ── 账户被禁用/封禁 ──
	if bytes.Contains(lower, []byte("account disabled")) ||
		bytes.Contains(lower, []byte("account suspended")) ||
		bytes.Contains(lower, []byte("account has been deactivated")) {
		return true, "account_disabled"
	}

	// ── 429 限流：不标记 key 耗尽 ──
	// 限流是临时性的，key 本身没问题。同账号的 key 共享 TPM/RPM 配额，
	// 换 key 也会被限，所以不标记、不换 key，直接返回 429 给客户端。
	// 包括：百炼 Throttling / Throttling.RateQuota / Throttling.BurstRate /
	// Throttling.AllocationQuota / insufficient_quota (Allocated quota exceeded)
	// OpenAI "Rate limit reached" / Claude rate_limit_error / DeepSeek "Rate Limit Reached"

	return false, ""
}

// wakeUpProbe 执行唤醒探测，遍历所有被屏蔽的 key 并尝试恢复。
// 返回第一个成功恢复的 key 索引，如果全部失败返回 -1。
func (s *Service) wakeUpProbe(r *http.Request, state *targetState) int {
	if state.keyPool == nil {
		return -1
	}

	target := state.Target()
	if target == nil {
		return -1
	}

	exhaustedKeys := state.keyPool.getProbeableExhaustedKeys()
	if len(exhaustedKeys) == 0 {
		// 没有可廉价探测的 key（全部为硬失败：无效/欠费/封禁/总额度耗尽）。
		// 这类 key 只能由冷却定时器被动恢复或真实请求成功确认，避免假阳性复活。
		return -1
	}

	s.logger.Info("[keypool] wake-up probe started",
		"request_id", appmiddleware.RequestIDFromContext(r.Context()),
		"target", target.Name,
		"exhausted_keys", len(exhaustedKeys),
	)

	// 对每个被屏蔽的 key 发送探测请求
	for _, idx := range exhaustedKeys {
		if s.probeKey(r.Context(), state, idx) {
			state.keyPool.markRecovered(idx)
			s.logger.Info("[keypool] wake-up probe succeeded",
				"request_id", appmiddleware.RequestIDFromContext(r.Context()),
				"target", target.Name,
				"key_index", idx,
			)
			return idx
		}
	}

	s.logger.Warn("[keypool] wake-up probe failed, all keys still exhausted",
		"request_id", appmiddleware.RequestIDFromContext(r.Context()),
		"target", target.Name,
	)
	return -1
}

// probeKey 对指定 key 发送轻量探测请求，验证 key 是否有效。
// 返回 true 表示 key 有效（探测成功），false 表示 key 仍无效。
func (s *Service) probeKey(ctx context.Context, state *targetState, keyIndex int) bool {
	target := state.Target()
	if target == nil || keyIndex < 0 || keyIndex >= len(state.keyPool.entries) {
		return false
	}

	apiKey := state.keyPool.entries[keyIndex].key

	// 构建探测请求 URL（GET /models 或 HEAD）
	probeURL := s.buildProbeURL(target)
	if probeURL == "" {
		return false
	}

	// 创建探测请求（短超时 5 秒）
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	probeReq, err := http.NewRequestWithContext(probeCtx, http.MethodGet, probeURL, nil)
	if err != nil {
		return false
	}

	// 注入上游凭证
	switch target.EndpointType {
	case config.EndpointTypeOpenAI, config.EndpointTypeDualProtocol:
		probeReq.Header.Set("Authorization", "Bearer "+apiKey)
	case config.EndpointTypeClaude:
		if target.AuthMode == "bearer" {
			probeReq.Header.Set("Authorization", "Bearer "+apiKey)
		} else {
			probeReq.Header.Set("x-api-key", apiKey)
		}
	case config.EndpointTypeGemini:
		probeReq.Header.Set("x-goog-api-key", apiKey)
	case config.EndpointTypeAzureOpenAI:
		probeReq.Header.Set("api-key", apiKey)
	default:
		probeReq.Header.Set("Authorization", "Bearer "+apiKey)
	}

	// 发送探测请求
	resp, err := s.httpClient.Do(probeReq)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	// 判断探测结果：只有 HTTP 200 才算 key 真实有效。
	// 为什么不接受 404/405/2xx 以外：某些上游（如百炼 compatible-mode 的 /v1/models）
	// 对任意 key 都返回 200，或路径不匹配返回 404——这些都无法证明 key 有效，
	// 接受它们会导致"探测假阳性 → 复活无效 key → 真实请求立刻又失败"的死循环。
	// 因此探测从严：非 200 一律视为未恢复，硬失败 key 交由冷却定时器被动恢复。
	return resp.StatusCode == http.StatusOK
}

// buildProbeURL 根据 endpoint_type 构建探测请求 URL。
func (s *Service) buildProbeURL(target *Target) string {
	if target == nil || target.Endpoint == nil {
		return ""
	}

	base := target.Endpoint.String()
	switch target.EndpointType {
	case config.EndpointTypeOpenAI:
		return base + "/v1/models"
	case config.EndpointTypeDualProtocol:
		u, err := s.buildURL(target, &url.URL{Path: "/v1/models"})
		if err != nil {
			return ""
		}
		return u.String()
	case config.EndpointTypeClaude:
		return base + "/v1/models"
	case config.EndpointTypeGemini:
		return base + "/v1beta/models"
	case config.EndpointTypeAzureOpenAI:
		// Azure 没有 /models 端点，用 HEAD 请求到根路径
		return base
	default:
		return base + "/v1/models"
	}
}
