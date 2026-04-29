// service.go — 代理服务核心编排：生命周期管理与请求主循环。
package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ycgame/llms-proxy/internal/auth"
	"github.com/ycgame/llms-proxy/internal/config"
	"github.com/ycgame/llms-proxy/internal/copilot"
	appmiddleware "github.com/ycgame/llms-proxy/internal/middleware"
	"github.com/ycgame/llms-proxy/internal/usage"
)

const headerProxyTarget = "X-Proxy-Target"

// Service forwards authenticated requests to upstream targets.
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

	affinity *affinityMap

	// Copilot 动态 token 相关（nil = 未配置）
	copilotService *copilot.CopilotService

	metrics   requestMetrics
	startTime time.Time
	rrCounter atomic.Uint64
}

// Target represents an upstream endpoint with runtime metadata.
type Target struct {
	Name               string
	EndpointType       string // azure_openai | openai | claude | gemini | wangsu_openai | wangsu_claude | wangsu_gemini
	Endpoint           *url.URL
	ResourcePathPrefix string
	APIKey             string
	AllowBearer        bool
	AllowedModels      []string
	SSEAutoAggregate   bool
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
		affinity:      newAffinityMap(),
		startTime:     time.Now(),
	}
	service.setRequestTimeout(time.Duration(cfg.Server.RequestTimeoutSeconds) * time.Second)

	if err := service.ApplyConfig(cfg); err != nil {
		return nil, err
	}

	return service, nil
}

// UpdateTargets refreshes the known targets from configuration.
func (s *Service) UpdateTargets(targets []config.Target) error {
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

	parsed, order, err := buildTargetStates(cfg.Targets)
	if err != nil {
		return err
	}

	timeout := time.Duration(cfg.Server.RequestTimeoutSeconds) * time.Second

	s.mu.Lock()
	s.targetsByName = parsed
	s.targetOrder = order
	s.mu.Unlock()

	s.setRequestTimeout(timeout)
	return nil
}

// SetUsageRecorder configures usage recorder for best-effort tracking.
func (s *Service) SetUsageRecorder(recorder usage.Recorder) {
	s.usageMu.Lock()
	s.usageRecorder = recorder
	s.usageMu.Unlock()
}

// SetCopilotService 注入 copilot 服务，启用 Copilot 动态 token 处理链。
// svc 为 nil 时 copilot 请求降级为 target.APIKey 静态认证。
func (s *Service) SetCopilotService(svc *copilot.CopilotService) {
	s.copilotService = svc
}

func (s *Service) currentUsageRecorder() usage.Recorder {
	s.usageMu.RLock()
	defer s.usageMu.RUnlock()
	return s.usageRecorder
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

	// Copilot 请求拦截：模型名以 Copilot 前缀开头且 copilotService 已配置时，
	// 走独立的 Copilot 处理链（动态 token、模型名映射、额度扣减）。
	if s.copilotService != nil && strings.HasPrefix(model, strings.ToLower(copilot.ModelPrefix)) {
		s.handleCopilotRequest(w, r, principal, bodyBytes, model)
		requestOutcomeRecorded = true
		return
	}

	for {
		attempt++
		state, err := s.selectTarget(principal, requestedLower, allowed, attempted, model, r.URL.Path, EndpointTypeHintFromContext(r.Context()), time.Now())
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

		// 对非 Azure target 的 multipart 请求，自动转换为 JSON。
		// 部分上游网关（如 aigateway）只接受 application/json，不支持 multipart/form-data。
		// 例外：网宿图像编辑端点（wangsu_openai_image_edit）原生要求 multipart/form-data，
		// 必须保留原样透传；wangsu_openai_image 也原生支持 multipart，同样保留。
		// Azure 端点保持原样透传 multipart（原生支持）。
		var origContentType string
		preserveMultipart := target.EndpointType == config.EndpointTypeAzureOpenAI ||
			target.EndpointType == config.EndpointTypeWangsuOpenAIImage ||
			target.EndpointType == config.EndpointTypeWangsuOpenAIImageEdit
		if !preserveMultipart && needsMultipartConvert(r) {
			if converted, newCT, convErr := convertMultipartToJSON(r, bodyBytes); convErr == nil {
				forwardBody = converted
				origContentType = r.Header.Get("Content-Type")
				r.Header.Set("Content-Type", newCT)
			} else {
				s.logger.Warn("multipart→JSON conversion failed, forwarding original",
					"request_id", appmiddleware.RequestIDFromContext(r.Context()),
					"target", target.Name,
					"error", convErr,
				)
			}
		}

		resp, cancel, fErr := s.forwardRequest(r, state, forwardBody)

		// 恢复原始 Content-Type（支持重试时路由到其他 target，如 Azure 需原始 multipart）
		if origContentType != "" {
			r.Header.Set("Content-Type", origContentType)
		}
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

		s.writeResponse(w, r, state, resp, cancel, attempt, model, forwardBody)
		// 更新连接粘连
		if principal != nil && target != nil {
			s.affinity.Set(affinityKey(principal.Name, model), strings.ToLower(target.Name), time.Now())
		}
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
		return 600 * time.Second
	}
	return timeout
}
