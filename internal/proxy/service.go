package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ycgame/azure-proxy/internal/auth"
	"github.com/ycgame/azure-proxy/internal/config"
	appmiddleware "github.com/ycgame/azure-proxy/internal/middleware"
	"github.com/ycgame/azure-proxy/internal/usage"
)

const (
	headerProxyTarget        = "X-Proxy-Target"
	headerAzureAuthorization = "X-Azure-Authorization"
)

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

	azureChatCompletionsFieldWhitelist = toFieldSet(
		"audio",
		"data_sources",
		"frequency_penalty",
		"function_call",
		"functions",
		"logit_bias",
		"logprobs",
		"max_completion_tokens",
		"max_tokens",
		"messages",
		"metadata",
		"modalities",
		"model",
		"n",
		"parallel_tool_calls",
		"prediction",
		"presence_penalty",
		"prompt_cache_key",
		"reasoning_effort",
		"response_format",
		"seed",
		"stop",
		"store",
		"stream",
		"stream_options",
		"temperature",
		"tool_choice",
		"tools",
		"top_logprobs",
		"top_p",
		"user",
		"user_security_context",
	)
	azureResponsesFieldWhitelist = toFieldSet(
		"background",
		"include",
		"input",
		"instructions",
		"max_output_tokens",
		"max_tool_calls",
		"metadata",
		"model",
		"parallel_tool_calls",
		"previous_response_id",
		"prompt",
		"prompt_cache_key",
		"reasoning",
		"store",
		"stream",
		"temperature",
		"text",
		"tool_choice",
		"tools",
		"top_logprobs",
		"top_p",
		"truncation",
		"user",
	)
	azureEmbeddingsFieldWhitelist = toFieldSet(
		"dimensions",
		"encoding_format",
		"input",
		"model",
		"user",
	)
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

// Service forwards authenticated requests to Azure targets.
type Service struct {
	logger         *slog.Logger
	httpClient     *http.Client
	requestTimeout atomic.Int64
	quietPeriod    time.Duration

	usageMu       sync.RWMutex
	usageRecorder usage.Recorder

	mu            sync.RWMutex
	targetsByName map[string]*targetState
	targetOrder   []*targetState

	metrics   requestMetrics
	startTime time.Time
	rrCounter atomic.Uint64
}

// Target represents an upstream endpoint with runtime metadata.
type Target struct {
	Name               string
	EndpointType       string // azure_openai | openai | claude
	Endpoint           *url.URL
	ResourcePathPrefix string
	APIKey             string
	AllowBearer        bool
	AllowedModels      []string
	allowedModelsSet   map[string]struct{}
}

type requestMetrics struct {
	totalRequests  atomic.Int64
	totalSuccess   atomic.Int64
	totalFailures  atomic.Int64
	totalRetries   atomic.Int64
	activeRequests atomic.Int64
}

// ServiceMetrics captures aggregate request statistics.
type ServiceMetrics struct {
	TotalRequests  int64
	TotalSuccess   int64
	TotalFailures  int64
	TotalRetries   int64
	ActiveRequests int64
	StartTime      time.Time
}

// TargetStatus summarizes the health of a configured target.
type TargetStatus struct {
	Name                 string
	EndpointType         string
	Endpoint             string
	ResourcePathPrefix   string
	Muted                bool
	MutedUntil           time.Time
	LastSuccess          time.Time
	LastFailure          time.Time
	ConsecutiveFailures  int
	TotalSuccessRequests int64
	TotalFailedRequests  int64
}

// NewService creates a proxy service from configuration.
func NewService(cfg *config.Config, logger *slog.Logger) (*Service, error) {
	if cfg == nil {
		return nil, errors.New("proxy: config must not be nil")
	}

	service := &Service{
		logger: logger,
		httpClient: &http.Client{
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				MaxIdleConns:          100,
				MaxIdleConnsPerHost:   50,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			},
		},
		quietPeriod:   60 * time.Second,
		targetsByName: make(map[string]*targetState),
		startTime:     time.Now(),
	}
	service.setRequestTimeout(time.Duration(cfg.Server.RequestTimeoutSeconds) * time.Second)

	if err := service.ApplyConfig(cfg); err != nil {
		return nil, err
	}

	return service, nil
}

// UpdateTargets refreshes the known targets from configuration.
func (s *Service) UpdateTargets(targets []config.AzureTarget) error {
	parsed, order, err := buildTargetStates(targets)
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.targetsByName = parsed
	s.targetOrder = order
	s.mu.Unlock()

	return nil
}

// ApplyConfig updates runtime settings based on a full configuration snapshot.
func (s *Service) ApplyConfig(cfg *config.Config) error {
	if cfg == nil {
		return errors.New("proxy: config must not be nil")
	}

	parsed, order, err := buildTargetStates(cfg.AzureTargets)
	if err != nil {
		return err
	}

	timeout := time.Duration(cfg.Server.RequestTimeoutSeconds) * time.Second

	s.mu.Lock()
	s.targetsByName = parsed
	s.targetOrder = order
	s.mu.Unlock()

	usagePath := strings.TrimSpace(cfg.DataFiles.UsageEventsFile)
	if usagePath != "" {
		s.SetUsageRecorder(usage.NewStore(usagePath))
	} else {
		s.SetUsageRecorder(nil)
	}

	s.setRequestTimeout(timeout)
	return nil
}

// SetUsageRecorder configures usage recorder for best-effort tracking.
func (s *Service) SetUsageRecorder(recorder usage.Recorder) {
	s.usageMu.Lock()
	s.usageRecorder = recorder
	s.usageMu.Unlock()
}

func (s *Service) currentUsageRecorder() usage.Recorder {
	s.usageMu.RLock()
	defer s.usageMu.RUnlock()
	return s.usageRecorder
}

func buildTargetStates(targets []config.AzureTarget) (map[string]*targetState, []*targetState, error) {
	if len(targets) == 0 {
		return make(map[string]*targetState), nil, nil
	}

	normalizeModels := func(list []string) []string {
		normalized := make([]string, 0, len(list))
		for _, m := range list {
			m = strings.ToLower(strings.TrimSpace(m))
			if m == "" {
				continue
			}
			normalized = append(normalized, m)
		}
		return normalized
	}

	parsed := make(map[string]*targetState, len(targets))
	order := make([]*targetState, 0, len(targets))
	for idx, t := range targets {
		endpoint, err := url.Parse(strings.TrimSpace(t.Endpoint))
		if err != nil {
			return nil, nil, fmt.Errorf("proxy: invalid endpoint for target %q: %w", t.Name, err)
		}
		if endpoint.Scheme == "" || endpoint.Host == "" {
			return nil, nil, fmt.Errorf("proxy: invalid endpoint for target %q: missing scheme or host", t.Name)
		}

		models := normalizeModels(t.AllowedModels)
		var modelSet map[string]struct{}
		if len(models) > 0 {
			modelSet = make(map[string]struct{}, len(models))
			for _, m := range models {
				modelSet[m] = struct{}{}
			}
		}

		info := &Target{
			Name:               strings.TrimSpace(t.Name),
			EndpointType:       config.NormalizeEndpointType(t.EndpointType),
			Endpoint:           endpoint,
			ResourcePathPrefix: normalizePrefix(t.ResourcePathPrefix),
			APIKey:             strings.TrimSpace(t.AzureAPIKey),
			AllowBearer:        t.AllowBearer,
			AllowedModels:      models,
			allowedModelsSet:   modelSet,
		}
		if info.Name == "" {
			return nil, nil, fmt.Errorf("proxy: target name at index %d must not be empty", idx)
		}
		if info.APIKey == "" && !info.AllowBearer {
			return nil, nil, fmt.Errorf("proxy: target %q missing azure_api_key and bearer passthrough not enabled", info.Name)
		}

		nameKey := strings.ToLower(info.Name)
		if _, exists := parsed[nameKey]; exists {
			return nil, nil, fmt.Errorf("proxy: duplicate target name %q", info.Name)
		}

		state := newTargetState(info)
		parsed[nameKey] = state
		order = append(order, state)
	}

	return parsed, order, nil
}

// ServeHTTP implements http.Handler for forwarding requests.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.metrics.totalRequests.Add(1)
	s.metrics.activeRequests.Add(1)
	requestOutcomeRecorded := false
	defer func() {
		if !requestOutcomeRecorded {
			s.metrics.totalFailures.Add(1)
		}
		s.metrics.activeRequests.Add(-1)
	}()

	principal, ok := auth.PrincipalFromContext(r.Context())
	if !ok || principal == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		requestOutcomeRecorded = true
		s.metrics.totalFailures.Add(1)
		return
	}

	// Fast-path: if no upstream targets are configured, inform the caller.
	s.mu.RLock()
	hasTargets := len(s.targetOrder) > 0
	s.mu.RUnlock()
	if !hasTargets {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"no upstream targets configured, add targets via admin UI"}`))
		requestOutcomeRecorded = true
		s.metrics.totalFailures.Add(1)
		return
	}

	requested := strings.TrimSpace(r.Header.Get(headerProxyTarget))
	if requested == "" {
		requested = strings.TrimSpace(r.URL.Query().Get("target"))
	}
	requestedLower := strings.ToLower(requested)
	allowFallback := requestedLower == ""

	var allowed map[string]struct{}
	if !principal.AllowAll() {
		list := principal.AllowedTargets()
		if len(list) > 0 {
			allowed = normalizeAllowed(list)
		}
	}

	bodyBytes, err := readAndBufferBody(r)
	if err != nil {
		s.logger.Error("failed to read request body",
			"request_id", appmiddleware.RequestIDFromContext(r.Context()),
			"error", err,
		)
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		requestOutcomeRecorded = true
		s.metrics.totalFailures.Add(1)
		return
	}

	if handled := s.maybeHandleLocalList(w, r); handled {
		requestOutcomeRecorded = true
		s.metrics.totalSuccess.Add(1)
		s.metrics.activeRequests.Add(-1)
		return
	}

	// Pre-compute Azure-sanitized body; the original body is preserved for non-Azure targets.
	sanitizedBody, strippedFields := sanitizeRequestBodyForAzure(r, bodyBytes)
	if len(strippedFields) > 0 {
		s.logger.Debug("stripped unsupported request fields",
			"request_id", appmiddleware.RequestIDFromContext(r.Context()),
			"path", r.URL.Path,
			"fields", strings.Join(strippedFields, ","),
		)
	}

	attempted := make(map[string]struct{})
	attempt := 0
	model := strings.ToLower(extractModel(r, bodyBytes))
	if model == "" && s.anyTargetRequiresModel() {
		http.Error(w, "model required when allowed_models are configured", http.StatusBadRequest)
		s.metrics.totalFailures.Add(1)
		requestOutcomeRecorded = true
		return
	}

	for {
		attempt++
		state, err := s.selectTarget(principal, requestedLower, allowed, attempted, model, time.Now())
		if err != nil {
			var selErr *selectionError
			if errors.As(err, &selErr) {
				http.Error(w, selErr.Message, selErr.Status)
				s.metrics.totalFailures.Add(1)
				requestOutcomeRecorded = true
				return
			}
			s.logger.Error("target selection failed",
				"request_id", appmiddleware.RequestIDFromContext(r.Context()),
				"error", err,
			)
			http.Error(w, "failed to select target", http.StatusBadGateway)
			s.metrics.totalFailures.Add(1)
			requestOutcomeRecorded = true
			return
		}

		target := state.Target()
		if target == nil {
			http.Error(w, "target unavailable", http.StatusBadGateway)
			s.metrics.totalFailures.Add(1)
			requestOutcomeRecorded = true
			return
		}

		if err := s.ensureModelAllowed(target, r, bodyBytes); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			s.metrics.totalFailures.Add(1)
			requestOutcomeRecorded = true
			return
		}

		targetKey := strings.ToLower(target.Name)
		attempted[targetKey] = struct{}{}

		// Use sanitized body only for Azure OpenAI targets; others get the original.
		forwardBody := bodyBytes
		if target.EndpointType == config.EndpointTypeAzureOpenAI {
			forwardBody = sanitizedBody
		}

		resp, cancel, fErr := s.forwardRequestWith503Retry(r, state, forwardBody)
		if fErr != nil {
			deferCancel(cancel)

			fe, ok := fErr.(*forwardAttemptError)
			status := http.StatusBadGateway
			if ok && fe.status != 0 {
				status = fe.status
			}

			if ok && fe.retryable {
				s.handleForwardError(r, state, fe.err, status)
				if !allowFallback {
					http.Error(w, http.StatusText(status), status)
					s.metrics.totalFailures.Add(1)
					requestOutcomeRecorded = true
					return
				}

				s.metrics.totalRetries.Add(1)
				s.logger.Info("retrying request with alternate target",
					"request_id", appmiddleware.RequestIDFromContext(r.Context()),
					"failed_target", target.Name,
					"status", status,
					"attempt", attempt,
				)
				continue
			}

			if ok {
				s.logger.Error("forward request failed",
					"request_id", appmiddleware.RequestIDFromContext(r.Context()),
					"target", target.Name,
					"error", fe.err,
					"status", status,
				)
			} else {
				s.logger.Error("forward request failed",
					"request_id", appmiddleware.RequestIDFromContext(r.Context()),
					"target", target.Name,
					"error", fErr,
					"status", status,
				)
			}
			http.Error(w, http.StatusText(status), status)
			s.metrics.totalFailures.Add(1)
			requestOutcomeRecorded = true
			return
		}

		s.writeResponse(w, r, state, resp, cancel, attempt, model)
		requestOutcomeRecorded = true
		return
	}
}

func readAndBufferBody(r *http.Request) ([]byte, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}

	data, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	if err := r.Body.Close(); err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	return data, nil
}

func deferCancel(cancel context.CancelFunc) {
	if cancel != nil {
		cancel()
	}
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
	default: // azure_openai
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
) {
	defer deferCancel(cancel)
	defer resp.Body.Close()

	target := state.Target()

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

type usageTokens struct {
	InputTokens  int64
	OutputTokens int64
	CachedTokens int64
}

func extractUsageTokens(contentType string, body []byte) (usageTokens, string, bool) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return usageTokens{}, "", false
	}

	contentType = strings.ToLower(strings.TrimSpace(contentType))
	if strings.Contains(contentType, "text/event-stream") || bytes.HasPrefix(body, []byte("data:")) {
		return extractUsageFromSSE(body)
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return usageTokens{}, "", false
	}
	return parseUsageFromPayload(payload)
}

func extractUsageFromSSE(body []byte) (usageTokens, string, bool) {
	var last usageTokens
	var model string
	found := false

	scanner := bufio.NewScanner(bytes.NewReader(body))
	buf := make([]byte, 0, 4096)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(strings.ToLower(line), "data:") {
			continue
		}
		chunk := strings.TrimSpace(line[len("data:"):])
		if chunk == "" || chunk == "[DONE]" {
			continue
		}

		var payload map[string]any
		if err := json.Unmarshal([]byte(chunk), &payload); err != nil {
			continue
		}
		tokens, m, ok := parseUsageFromPayload(payload)
		if !ok {
			continue
		}
		last = tokens
		if m != "" {
			model = m
		}
		found = true
	}

	if !found {
		return usageTokens{}, "", false
	}
	return last, model, true
}

func parseUsageFromPayload(payload map[string]any) (usageTokens, string, bool) {
	if payload == nil {
		return usageTokens{}, "", false
	}

	model := strings.ToLower(readString(payload["model"]))

	if usageMap, ok := payload["usage"].(map[string]any); ok {
		tokens, found := parseUsageMap(usageMap)
		if found {
			return tokens, model, true
		}
	}

	if responseMap, ok := payload["response"].(map[string]any); ok {
		if model == "" {
			model = strings.ToLower(readString(responseMap["model"]))
		}
		if usageMap, ok := responseMap["usage"].(map[string]any); ok {
			tokens, found := parseUsageMap(usageMap)
			if found {
				return tokens, model, true
			}
		}
	}

	if _, hasPrompt := payload["prompt_tokens"]; hasPrompt {
		return usageTokens{
			InputTokens:  readInt64(payload["prompt_tokens"]),
			OutputTokens: readInt64(payload["completion_tokens"]),
		}, model, true
	}

	return usageTokens{}, model, false
}

func parseUsageMap(usageMap map[string]any) (usageTokens, bool) {
	if usageMap == nil {
		return usageTokens{}, false
	}

	hasAny := false
	readField := func(names ...string) (int64, bool) {
		for _, name := range names {
			if value, ok := usageMap[name]; ok {
				hasAny = true
				return readInt64(value), true
			}
		}
		return 0, false
	}

	input, _ := readField("input_tokens", "prompt_tokens")
	output, _ := readField("output_tokens", "completion_tokens")
	cached, hasCached := readField("cached_tokens")
	if !hasCached {
		if details, ok := usageMap["input_tokens_details"].(map[string]any); ok {
			if value, ok := details["cached_tokens"]; ok {
				hasAny = true
				cached = readInt64(value)
			}
		}
		if details, ok := usageMap["prompt_tokens_details"].(map[string]any); ok {
			if value, ok := details["cached_tokens"]; ok {
				hasAny = true
				cached = readInt64(value)
			}
		}
	}

	if !hasAny {
		return usageTokens{}, false
	}

	return usageTokens{
		InputTokens:  input,
		OutputTokens: output,
		CachedTokens: cached,
	}, true
}

func readInt64(value any) int64 {
	switch v := value.(type) {
	case float64:
		return int64(v)
	case float32:
		return int64(v)
	case int64:
		return v
	case int32:
		return int64(v)
	case int:
		return int64(v)
	case json.Number:
		n, err := v.Int64()
		if err == nil {
			return n
		}
		f, err := v.Float64()
		if err == nil {
			return int64(f)
		}
	case string:
		var n json.Number = json.Number(strings.TrimSpace(v))
		if iv, err := n.Int64(); err == nil {
			return iv
		}
	}
	return 0
}

func readString(value any) string {
	if value == nil {
		return ""
	}
	if s, ok := value.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

type limitedCaptureWriter struct {
	limit int
	buf   []byte
}

func (w *limitedCaptureWriter) Write(p []byte) (int, error) {
	if w.limit <= 0 {
		return len(p), nil
	}
	if len(p) >= w.limit {
		w.buf = append(w.buf[:0], p[len(p)-w.limit:]...)
		return len(p), nil
	}
	combined := append(append(make([]byte, 0, len(w.buf)+len(p)), w.buf...), p...)
	if len(combined) > w.limit {
		combined = combined[len(combined)-w.limit:]
	}
	w.buf = combined
	return len(p), nil
}

func (w *limitedCaptureWriter) Bytes() []byte {
	if len(w.buf) == 0 {
		return nil
	}
	out := make([]byte, len(w.buf))
	copy(out, w.buf)
	return out
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

func (s *Service) selectTarget(
	principal *auth.Principal,
	requestedLower string,
	allowed map[string]struct{},
	attempted map[string]struct{},
	model string,
	now time.Time,
) (*targetState, error) {
	if requestedLower != "" {
		if !principal.CanAccess(requestedLower) {
			return nil, newSelectionError(http.StatusForbidden, "target not allowed")
		}
		state, ok := s.targetByName(requestedLower)
		if !ok {
			return nil, newSelectionError(http.StatusBadRequest, "unknown target")
		}
		if !modelAllowed(state.Target(), model) {
			return nil, newSelectionError(http.StatusBadRequest, fmt.Sprintf("model %q not allowed for target %q", model, state.Target().Name))
		}
		if _, tried := attempted[strings.ToLower(state.Target().Name)]; tried {
			return nil, newSelectionError(http.StatusServiceUnavailable, "requested target already attempted")
		}
		if state.IsMuted(now) {
			return nil, newSelectionError(http.StatusServiceUnavailable, "requested target temporarily unavailable")
		}
		return state, nil
	}

	candidate, mutedCandidate := s.findAvailableTargetWithModel(allowed, attempted, model, now)
	if candidate != nil {
		return candidate, nil
	}

	if mutedCandidate != nil {
		return nil, newSelectionError(http.StatusServiceUnavailable, fmt.Sprintf("all targets muted until %s", mutedCandidate.NextRetry().Format(time.RFC3339)))
	}

	if len(attempted) > 0 {
		return nil, newSelectionError(http.StatusBadGateway, "no alternative target available")
	}

	if model != "" {
		return nil, newSelectionError(http.StatusBadRequest, "model not supported by any target")
	}

	return nil, newSelectionError(http.StatusBadGateway, "no target available")
}

func (s *Service) targetByName(name string) (*targetState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	target, ok := s.targetsByName[strings.ToLower(name)]
	return target, ok
}

func (s *Service) targetSnapshot() []*targetState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snapshot := make([]*targetState, len(s.targetOrder))
	copy(snapshot, s.targetOrder)
	return snapshot
}

// MetricsSnapshot returns a copy of the service-level metrics counters.
func (s *Service) MetricsSnapshot() ServiceMetrics {
	return ServiceMetrics{
		TotalRequests:  s.metrics.totalRequests.Load(),
		TotalSuccess:   s.metrics.totalSuccess.Load(),
		TotalFailures:  s.metrics.totalFailures.Load(),
		TotalRetries:   s.metrics.totalRetries.Load(),
		ActiveRequests: s.metrics.activeRequests.Load(),
		StartTime:      s.startTime,
	}
}

// TargetStatuses provides a read-only snapshot of target health information.
func (s *Service) TargetStatuses(now time.Time) []TargetStatus {
	snapshot := s.targetSnapshot()
	statuses := make([]TargetStatus, 0, len(snapshot))
	for _, state := range snapshot {
		if state == nil {
			continue
		}
		target := state.Target()
		if target == nil {
			continue
		}
		stats := state.Stats()
		endpoint := ""
		if target.Endpoint != nil {
			endpoint = target.Endpoint.String()
		}
		statuses = append(statuses, TargetStatus{
			Name:                 target.Name,
			EndpointType:         target.EndpointType,
			Endpoint:             endpoint,
			ResourcePathPrefix:   target.ResourcePathPrefix,
			Muted:                state.IsMuted(now),
			MutedUntil:           stats.MutedUntil,
			LastSuccess:          stats.LastSuccess,
			LastFailure:          stats.LastFailure,
			ConsecutiveFailures:  stats.ConsecutiveFailure,
			TotalSuccessRequests: stats.TotalSuccess,
			TotalFailedRequests:  stats.TotalFailure,
		})
	}
	return statuses
}

// StartTime returns the instant the service was constructed.
func (s *Service) StartTime() time.Time {
	return s.startTime
}

func (s *Service) buildURL(target *Target, original *url.URL) (*url.URL, error) {
	if target == nil || target.Endpoint == nil {
		return nil, errors.New("target not configured")
	}

	path := mergePaths(target.ResourcePathPrefix, original.Path)

	forward := *target.Endpoint
	forward.RawQuery = ""
	forward.Fragment = ""

	// Concatenate paths explicitly instead of using url.URL.Parse, because
	// url.Parse treats paths starting with "/" as absolute and would discard
	// any sub-path already present in the endpoint (e.g. a gateway base path
	// like /v2/gws/<id>/anthropic).
	forward.Path = strings.TrimRight(forward.Path, "/") + "/" + strings.TrimLeft(path, "/")
	forward.RawQuery = normalizeForwardQuery(original)
	return &forward, nil
}

func normalizeForwardQuery(original *url.URL) string {
	if original == nil {
		return ""
	}

	query := original.Query()
	deleteQueryKeyCaseInsensitive(query, "target")
	deleteQueryKeyCaseInsensitive(query, "api-version")
	deleteQueryKeyCaseInsensitive(query, "api_version")
	deleteQueryKeyCaseInsensitive(query, "api-key")

	return query.Encode()
}

func deleteQueryKeyCaseInsensitive(query url.Values, key string) {
	for existing := range query {
		if strings.EqualFold(existing, key) {
			delete(query, existing)
		}
	}
}

func toFieldSet(fields ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		field = strings.ToLower(strings.TrimSpace(field))
		if field == "" {
			continue
		}
		set[field] = struct{}{}
	}
	return set
}

func whitelistForPath(path string) (map[string]struct{}, bool) {
	path = strings.ToLower(strings.TrimSpace(path))
	if path == "" {
		return nil, false
	}

	if strings.HasSuffix(path, "/chat/completions") {
		return azureChatCompletionsFieldWhitelist, true
	}
	if strings.HasSuffix(path, "/responses") {
		return azureResponsesFieldWhitelist, true
	}
	if strings.HasSuffix(path, "/embeddings") {
		return azureEmbeddingsFieldWhitelist, true
	}

	return nil, false
}

func sanitizeRequestBodyForAzure(r *http.Request, body []byte) ([]byte, []string) {
	if r == nil || len(body) == 0 {
		return body, nil
	}

	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
	default:
		return body, nil
	}

	fieldWhitelist, ok := whitelistForPath(r.URL.Path)
	if !ok {
		return body, nil
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, nil
	}

	stripped := make([]string, 0)
	for key := range payload {
		keyLower := strings.ToLower(strings.TrimSpace(key))
		if _, allowed := fieldWhitelist[keyLower]; allowed {
			continue
		}
		delete(payload, key)
		stripped = append(stripped, key)
	}

	if len(stripped) == 0 {
		return body, nil
	}

	filtered, err := json.Marshal(payload)
	if err != nil {
		return body, nil
	}

	sort.Strings(stripped)
	return filtered, stripped
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

func (s *Service) setRequestTimeout(timeout time.Duration) {
	timeout = normalizeRequestTimeout(timeout)
	s.requestTimeout.Store(timeout.Nanoseconds())
}

func (s *Service) getRequestTimeout() time.Duration {
	nanos := s.requestTimeout.Load()
	if nanos <= 0 {
		return normalizeRequestTimeout(0)
	}
	return time.Duration(nanos)
}

func normalizeRequestTimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return 300 * time.Second
	}
	return timeout
}

func normalizePrefix(prefix string) string {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return ""
	}
	if !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}
	return strings.TrimRight(prefix, "/")
}

func mergePaths(prefix, path string) string {
	if prefix == "" {
		if path == "" {
			return "/"
		}
		return path
	}
	if path == "" || path == "/" {
		return prefix
	}
	if strings.HasPrefix(path, prefix+"/") || path == prefix {
		return path
	}
	return strings.TrimRight(prefix, "/") + "/" + strings.TrimLeft(path, "/")
}

func modelAllowed(t *Target, model string) bool {
	if t == nil {
		return false
	}
	if len(t.AllowedModels) == 0 {
		return true
	}
	modelKey := strings.ToLower(strings.TrimSpace(model))
	if modelKey == "" {
		return false
	}
	if t.allowedModelsSet != nil {
		_, ok := t.allowedModelsSet[modelKey]
		return ok
	}
	for _, m := range t.AllowedModels {
		if strings.EqualFold(m, modelKey) {
			return true
		}
	}
	return false
}

func (s *Service) anyTargetRequiresModel() bool {
	snapshot := s.targetSnapshot()
	for _, state := range snapshot {
		if state == nil || state.Target() == nil {
			continue
		}
		if len(state.Target().AllowedModels) > 0 {
			return true
		}
	}
	return false
}

func (s *Service) ensureModelAllowed(target *Target, r *http.Request, body []byte) error {
	if target == nil || len(target.AllowedModels) == 0 {
		return nil
	}

	model := strings.ToLower(extractModel(r, body))
	if model == "" {
		return errors.New("model required when allowed_models is configured")
	}
	if modelAllowed(target, model) {
		return nil
	}
	return fmt.Errorf("model %q not allowed for target %q", model, target.Name)
}

func extractModel(r *http.Request, body []byte) string {
	if r == nil {
		return ""
	}

	pathLower := strings.ToLower(r.URL.Path)
	if isListEndpoint(pathLower) {
		return ""
	}

	path := strings.ToLower(r.URL.Path)
	const deploymentsSegment = "/deployments/"
	if idx := strings.Index(path, deploymentsSegment); idx >= 0 {
		after := path[idx+len(deploymentsSegment):]
		if slash := strings.Index(after, "/"); slash >= 0 {
			after = after[:slash]
		}
		return strings.TrimSpace(after)
	}

	if len(body) > 0 {
		contentType := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
		if strings.Contains(contentType, "application/x-www-form-urlencoded") {
			if vals, err := url.ParseQuery(string(body)); err == nil {
				if model := strings.TrimSpace(vals.Get("model")); model != "" {
					return model
				}
			}
		}

		if strings.Contains(contentType, "multipart/form-data") {
			if _, params, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err == nil {
				if boundary := strings.TrimSpace(params["boundary"]); boundary != "" {
					if model := extractMultipartModel(body, boundary); model != "" {
						return model
					}
				}
			}
		}

		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err == nil {
			if m, ok := payload["model"].(string); ok {
				return strings.TrimSpace(m)
			}
		}
	}
	return ""
}

func extractMultipartModel(body []byte, boundary string) string {
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			return ""
		}
		if err != nil {
			return ""
		}

		if strings.EqualFold(part.FormName(), "model") {
			data, err := io.ReadAll(io.LimitReader(part, 1024))
			_ = part.Close()
			if err != nil {
				return ""
			}
			return strings.TrimSpace(string(data))
		}

		_, _ = io.Copy(io.Discard, part)
		_ = part.Close()
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

func normalizeAllowed(list []string) map[string]struct{} {
	m := make(map[string]struct{}, len(list))
	for _, item := range list {
		item = strings.ToLower(strings.TrimSpace(item))
		if item == "" {
			continue
		}
		m[item] = struct{}{}
	}
	return m
}

func (s *Service) findAvailableTarget(allowed map[string]struct{}, attempted map[string]struct{}, now time.Time) (*targetState, *targetState) {
	return s.findAvailableTargetWithModel(allowed, attempted, "", now)
}

func isListEndpoint(path string) bool {
	path = strings.ToLower(path)
	return path == "/openai/deployments" ||
		path == "/openai/models" ||
		path == "/models" ||
		path == "/v1/models"
}

func (s *Service) maybeHandleLocalList(w http.ResponseWriter, r *http.Request) bool {
	if r == nil || r.Method != http.MethodGet {
		return false
	}
	path := strings.ToLower(r.URL.Path)
	if !isListEndpoint(path) {
		return false
	}

	principal, ok := auth.PrincipalFromContext(r.Context())
	if !ok || principal == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return true
	}

	var targetFilter map[string]struct{}
	if !principal.AllowAll() {
		targetFilter = normalizeAllowed(principal.AllowedTargets())
	}

	requested := strings.TrimSpace(r.Header.Get(headerProxyTarget))
	if requested == "" {
		requested = strings.TrimSpace(r.URL.Query().Get("target"))
	}
	if requested != "" {
		requestedLower := strings.ToLower(requested)
		if !principal.CanAccess(requestedLower) {
			http.Error(w, "target not allowed", http.StatusForbidden)
			return true
		}
		if _, exists := s.targetByName(requestedLower); !exists {
			http.Error(w, "unknown target", http.StatusBadRequest)
			return true
		}
		targetFilter = map[string]struct{}{requestedLower: {}}
	}

	items := s.buildLocalDeployments(targetFilter)
	resp := map[string]any{
		"object": "list",
		"data":   items,
		"first_id": func() string {
			if len(items) == 0 {
				return ""
			}
			if id, ok := items[0]["id"].(string); ok {
				return id
			}
			return ""
		}(),
		"last_id": func() string {
			if len(items) == 0 {
				return ""
			}
			if id, ok := items[len(items)-1]["id"].(string); ok {
				return id
			}
			return ""
		}(),
		"has_more": false,
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		s.logger.Warn("failed to encode local list response",
			"request_id", appmiddleware.RequestIDFromContext(r.Context()),
			"error", err,
		)
		http.Error(w, "failed to build response", http.StatusInternalServerError)
		return true
	}
	return true
}

func (s *Service) buildLocalDeployments(targetFilter map[string]struct{}) []map[string]any {
	seen := make(map[string]struct{})
	result := make([]map[string]any, 0)
	snapshot := s.targetSnapshot()
	for _, state := range snapshot {
		if state == nil || state.Target() == nil {
			continue
		}
		target := state.Target()
		if targetFilter != nil {
			targetKey := strings.ToLower(strings.TrimSpace(target.Name))
			if _, allowed := targetFilter[targetKey]; !allowed {
				continue
			}
		}
		models := target.AllowedModels
		for _, m := range models {
			m = strings.TrimSpace(m)
			if m == "" {
				continue
			}
			key := strings.ToLower(m)
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			result = append(result, map[string]any{
				"object":          "deployment",
				"id":              m,
				"model":           m,
				"status":          "succeeded",
				"created_at":      time.Now().Unix(),
				"deployed_tokens": nil,
			})
		}
	}
	// deterministic order
	sort.Slice(result, func(i, j int) bool {
		return strings.ToLower(result[i]["id"].(string)) < strings.ToLower(result[j]["id"].(string))
	})
	return result
}

func (s *Service) findAvailableTargetWithModel(allowed map[string]struct{}, attempted map[string]struct{}, model string, now time.Time) (*targetState, *targetState) {
	var mutedCandidate *targetState
	snapshot := s.targetSnapshot()
	if len(snapshot) == 0 {
		return nil, nil
	}
	start := int(s.rrCounter.Add(1)-1) % len(snapshot)
	for i := 0; i < len(snapshot); i++ {
		state := snapshot[(start+i)%len(snapshot)]
		if state == nil || state.Target() == nil {
			continue
		}
		name := strings.ToLower(state.Target().Name)
		if attempted != nil {
			if _, seen := attempted[name]; seen {
				continue
			}
		}
		if allowed != nil {
			if _, ok := allowed[name]; !ok {
				continue
			}
		}
		if !modelAllowed(state.Target(), model) {
			continue
		}

		if !state.IsMuted(now) {
			return state, mutedCandidate
		}

		if mutedCandidate == nil || state.NextRetry().Before(mutedCandidate.NextRetry()) {
			mutedCandidate = state
		}
	}
	return nil, mutedCandidate
}

type targetState struct {
	target *Target

	mu                 sync.RWMutex
	mutedUntil         time.Time
	lastSuccess        time.Time
	lastFailure        time.Time
	consecutiveFailure int
	totalSuccess       int64
	totalFailure       int64
}

// TargetStats is a snapshot of target runtime metrics.
type TargetStats struct {
	MutedUntil         time.Time
	LastSuccess        time.Time
	LastFailure        time.Time
	ConsecutiveFailure int
	TotalSuccess       int64
	TotalFailure       int64
}

func newTargetState(t *Target) *targetState {
	return &targetState{
		target: t,
	}
}

func (s *targetState) Target() *Target {
	return s.target
}

func (s *targetState) IsMuted(now time.Time) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mutedUntil.After(now)
}

func (s *targetState) NextRetry() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mutedUntil
}

func (s *targetState) MarkFailure(now time.Time, quietPeriod time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastFailure = now
	s.consecutiveFailure++
	s.totalFailure++
	if quietPeriod <= 0 {
		quietPeriod = 60 * time.Second
	}
	s.mutedUntil = now.Add(quietPeriod)
}

func (s *targetState) MarkSuccess(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastSuccess = now
	s.consecutiveFailure = 0
	s.totalSuccess++
	s.mutedUntil = time.Time{}
}

// Stats returns a snapshot of the target state metrics.
func (s *targetState) Stats() TargetStats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return TargetStats{
		MutedUntil:         s.mutedUntil,
		LastSuccess:        s.lastSuccess,
		LastFailure:        s.lastFailure,
		ConsecutiveFailure: s.consecutiveFailure,
		TotalSuccess:       s.totalSuccess,
		TotalFailure:       s.totalFailure,
	}
}

type selectionError struct {
	Status  int
	Message string
}

func (e *selectionError) Error() string {
	return e.Message
}

func newSelectionError(status int, message string) error {
	return &selectionError{
		Status:  status,
		Message: message,
	}
}
