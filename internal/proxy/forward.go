// forward.go — HTTP 转发、协议认证注入、响应写回与重试逻辑。
package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/ycgame/llms-proxy/internal/auth"
	"github.com/ycgame/llms-proxy/internal/config"
	appmiddleware "github.com/ycgame/llms-proxy/internal/middleware"
	"github.com/ycgame/llms-proxy/internal/usage"
)

const headerAzureAuthorization = "X-Azure-Authorization"

var hopHeaders = map[string]struct{}{
	"connection":          {},
	"proxy-connection":    {},
	"keep-alive":          {},
	"proxy-authenticate":  {},
	"proxy-authorization": {},
	"te":                  {},
	"trailers":            {},
	"transfer-encoding":   {},
	"upgrade":             {},
}

var (
	upstream503MaxRetries  = 2
	upstream503RetryDelay  = time.Second
	upstream503RetryJitter = 300 * time.Millisecond
)

type forwardAttemptError struct {
	status    int
	retryable bool
	err       error
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

func (s *Service) forwardRequest(r *http.Request, state *targetState, body []byte) (*http.Response, context.CancelFunc, error) {
	target := state.Target()
	if target == nil {
		return nil, nil, &forwardAttemptError{
			status:    http.StatusBadGateway,
			retryable: false,
			err:       errors.New("target not configured"),
		}
	}

	azureAuth := strings.TrimSpace(r.Header.Get(headerAzureAuthorization))
	forwardURL, err := s.buildURL(target, r.URL)
	if err != nil {
		return nil, nil, &forwardAttemptError{
			status:    http.StatusBadGateway,
			retryable: false,
			err:       err,
		}
	}

	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.getRequestTimeout())

	req, err := http.NewRequestWithContext(ctx, r.Method, forwardURL.String(), bodyReader)
	if err != nil {
		cancel()
		return nil, nil, &forwardAttemptError{
			status:    http.StatusBadRequest,
			retryable: false,
			err:       err,
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
		req.Header.Set("Authorization", "Bearer "+target.APIKey)
	case config.EndpointTypeClaude:
		req.Header.Set("x-api-key", target.APIKey)
		if req.Header.Get("anthropic-version") == "" {
			req.Header.Set("anthropic-version", "2023-06-01")
		}
	case config.EndpointTypeGemini:
		req.Header.Set("x-goog-api-key", target.APIKey)
	case config.EndpointTypeWangsuOpenAI:
		req.Header.Set("Authorization", "Bearer "+target.APIKey)
	case config.EndpointTypeWangsuClaude:
		req.Header.Set("x-api-key", target.APIKey)
		if req.Header.Get("anthropic-version") == "" {
			req.Header.Set("anthropic-version", "2023-06-01")
		}
	case config.EndpointTypeWangsuGemini:
		req.Header.Set("x-goog-api-key", target.APIKey)
	case config.EndpointTypeAzureOpenAI:
		useBearer := target.AllowBearer && azureAuth != ""
		if useBearer {
			req.Header.Set("Authorization", azureAuth)
		} else {
			if target.APIKey == "" {
				cancel()
				return nil, nil, &forwardAttemptError{
					status:    http.StatusBadRequest,
					retryable: false,
					err:       errors.New("proxy: missing azure credential; provide api key or X-Azure-Authorization"),
				}
			}
			req.Header.Set("api-key", target.APIKey)
		}
	default:
		cancel()
		return nil, nil, &forwardAttemptError{
			status:    http.StatusInternalServerError,
			retryable: false,
			err:       fmt.Errorf("proxy: unsupported endpoint type %q for target %q", target.EndpointType, target.Name),
		}
	}
	req.Host = target.Endpoint.Host

	resp, err := s.httpClient.Do(req)
	if err != nil {
		cancel()
		retryable := !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled)
		return nil, nil, &forwardAttemptError{
			status:    classifyTransportError(err),
			retryable: retryable,
			err:       err,
		}
	}

	return resp, cancel, nil
}

func (s *Service) forwardRequestWith503Retry(r *http.Request, state *targetState, body []byte) (*http.Response, context.CancelFunc, error) {
	retries := 0
	for {
		resp, cancel, err := s.forwardRequest(r, state, body)
		if err != nil {
			return nil, nil, err
		}
		if resp.StatusCode != http.StatusServiceUnavailable || retries >= upstream503MaxRetries {
			return resp, cancel, nil
		}

		_ = resp.Body.Close()
		deferCancel(cancel)

		retries++
		s.metrics.totalRetries.Add(1)
		delay := retryDelayWithJitter(upstream503RetryDelay, upstream503RetryJitter)
		s.logger.Info("retrying request after upstream 503",
			"request_id", appmiddleware.RequestIDFromContext(r.Context()),
			"target", targetName(state.Target()),
			"retry", retries,
			"max_retries", upstream503MaxRetries,
			"delay_ms", delay.Milliseconds(),
		)

		if err := sleepWithContext(r.Context(), delay); err != nil {
			return nil, nil, &forwardAttemptError{
				status:    classifyTransportError(err),
				retryable: false,
				err:       err,
			}
		}
	}
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
						if _, skip := hopHeaders[strings.ToLower(key)]; skip {
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
		if _, skip := hopHeaders[strings.ToLower(key)]; skip {
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

	if resp.StatusCode >= 500 {
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

func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		if _, skip := hopHeaders[strings.ToLower(key)]; skip {
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

func retryDelayWithJitter(base, jitter time.Duration) time.Duration {
	if base < 0 {
		base = 0
	}
	if jitter <= 0 {
		return base
	}
	nanos := time.Now().UnixNano()
	if nanos < 0 {
		nanos = -nanos
	}
	return base + time.Duration(nanos%int64(jitter+1))
}

func sleepWithContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func targetName(t *Target) string {
	if t == nil {
		return ""
	}
	return t.Name
}
