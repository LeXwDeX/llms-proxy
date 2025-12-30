package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
	requestTimeout time.Duration
	quietPeriod    time.Duration

	mu            sync.RWMutex
	targetsByName map[string]*targetState
	targetOrder   []*targetState

	metrics   requestMetrics
	startTime time.Time
	rrCounter atomic.Uint64
}

// Target represents an Azure endpoint with runtime metadata.
type Target struct {
	Name               string
	Endpoint           *url.URL
	ResourcePathPrefix string
	APIKey             string
	AllowBearer        bool
	AllowedModels      []string
	DefaultAPIVersion  string
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
				MaxIdleConns:          64,
				MaxIdleConnsPerHost:   32,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			},
		},
		requestTimeout: time.Duration(cfg.Server.RequestTimeoutSeconds) * time.Second,
		quietPeriod:    60 * time.Second,
		targetsByName:  make(map[string]*targetState),
		startTime:      time.Now(),
	}

	if service.requestTimeout <= 0 {
		service.requestTimeout = 300 * time.Second
	}

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
	if timeout <= 0 {
		timeout = 300 * time.Second
	}

	s.mu.Lock()
	s.targetsByName = parsed
	s.targetOrder = order
	s.mu.Unlock()

	s.requestTimeout = timeout
	return nil
}

func buildTargetStates(targets []config.AzureTarget) (map[string]*targetState, []*targetState, error) {
	if len(targets) == 0 {
		return nil, nil, errors.New("proxy: no azure targets configured")
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

		info := &Target{
			Name:               strings.TrimSpace(t.Name),
			Endpoint:           endpoint,
			ResourcePathPrefix: normalizePrefix(t.ResourcePathPrefix),
			APIKey:             strings.TrimSpace(t.AzureAPIKey),
			AllowBearer:        t.AllowBearer,
			AllowedModels:      normalizeModels(t.AllowedModels),
			DefaultAPIVersion:  strings.TrimSpace(t.DefaultAPIVersion),
		}
		if info.Name == "" {
			return nil, nil, fmt.Errorf("proxy: target name at index %d must not be empty", idx)
		}
		if info.APIKey == "" && !info.AllowBearer {
			return nil, nil, fmt.Errorf("proxy: target %q missing azure_api_key and bearer passthrough not enabled", info.Name)
		}
		if info.DefaultAPIVersion == "" {
			return nil, nil, fmt.Errorf("proxy: target %q missing default_api_version", info.Name)
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

		resp, cancel, fErr := s.forwardRequest(r, state, bodyBytes)
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

		s.writeResponse(w, r, state, resp, cancel, attempt)
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

	ctx, cancel := context.WithTimeout(r.Context(), s.requestTimeout)

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

func (s *Service) writeResponse(
	w http.ResponseWriter,
	r *http.Request,
	state *targetState,
	resp *http.Response,
	cancel context.CancelFunc,
	attempt int,
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
		w.Header().Set("X-Azure-Target", target.Name)
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
	if _, err := io.Copy(writer, resp.Body); err != nil &&
		!errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded) {
		s.logger.Warn("stream copy failed",
			"request_id", appmiddleware.RequestIDFromContext(r.Context()),
			"target", targetName(target),
			"error", err,
		)
	}
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

	endpointCopy := *target.Endpoint
	endpointCopy.RawQuery = ""
	endpointCopy.Fragment = ""

	forward, err := endpointCopy.Parse(path)
	if err != nil {
		return nil, err
	}
	query := original.Query()
	query.Del("api-key")
	if target.DefaultAPIVersion != "" {
		query.Set("api-version", target.DefaultAPIVersion)
	}
	forward.RawQuery = query.Encode()
	return forward, nil
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
	if model == "" {
		return false
	}
	for _, m := range t.AllowedModels {
		if strings.EqualFold(m, model) {
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
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err == nil {
			if m, ok := payload["model"].(string); ok {
				return strings.TrimSpace(m)
			}
		}
	}
	return ""
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

	requested := strings.TrimSpace(r.Header.Get(headerProxyTarget))
	if requested == "" {
		requested = strings.TrimSpace(r.URL.Query().Get("target"))
	}
	if requested != "" {
		principal, ok := auth.PrincipalFromContext(r.Context())
		if !ok || principal == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return true
		}
		if !principal.CanAccess(requested) {
			http.Error(w, "target not allowed", http.StatusForbidden)
			return true
		}
		if _, exists := s.targetByName(requested); !exists {
			http.Error(w, "unknown target", http.StatusBadRequest)
			return true
		}
	}

	items := s.buildLocalDeployments()
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

func (s *Service) buildLocalDeployments() []map[string]any {
	seen := make(map[string]struct{})
	result := make([]map[string]any, 0)
	snapshot := s.targetSnapshot()
	for _, state := range snapshot {
		if state == nil || state.Target() == nil {
			continue
		}
		target := state.Target()
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
