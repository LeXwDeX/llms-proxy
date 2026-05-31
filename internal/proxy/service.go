// service.go — 代理服务核心编排：生命周期管理与请求主循环。
package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ycgame/llms-proxy/internal/auth"
	"github.com/ycgame/llms-proxy/internal/config"
	"github.com/ycgame/llms-proxy/internal/copilot"
	appmiddleware "github.com/ycgame/llms-proxy/internal/middleware"
	"github.com/ycgame/llms-proxy/internal/tracestore"
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

	// Trace store for DEBUG mode (nil = disabled)
	traceStore *tracestore.Store

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
	APIKeys            []string // 合并后的有序 key 池 [api_key, api_keys...]
	KeyResetTime       string   // 额度重置时间点（CST）
	AllowBearer        bool
	AuthMode           string
	AllowedModels      []string
	SSEAutoAggregate   bool
	allowedModelsSet   map[string]struct{}
}

type requestMetrics struct {
	totalRequests      atomic.Int64
	totalSuccess       atomic.Int64
	totalFailures      atomic.Int64
	totalKeyRetries    atomic.Int64 // key pool 内换 key 重试
	totalTargetRetries atomic.Int64 // target 级 failover 重试
	activeRequests     atomic.Int64
}

// ServiceMetrics captures aggregate request statistics.
type ServiceMetrics struct {
	TotalRequests      int64
	TotalSuccess       int64
	TotalFailures      int64
	TotalRetries       int64 // 总重试 = KeyRetries + TargetRetries（向后兼容）
	TotalKeyRetries    int64 // key pool 内换 key 重试次数
	TotalTargetRetries int64 // target 级 failover 重试次数
	ActiveRequests     int64
	StartTime          time.Time
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
	// Key pool status (only populated if target has multiple keys)
	TotalKeys                 int
	ActiveKeys                int
	ExhaustedSubscriptionKeys int // quota_exceeded_subscription (自动恢复)
	ExhaustedAPIKeys          int // quota_exceeded_api (人工恢复)
	RateLimitedKeys           int
	BlockedKeys               int
	OtherExhaustedKeys        int // 其他失效原因 (invalid_token/billing_error/account_disabled/旧版 quota_exceeded)，5 分钟冷却自动恢复
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

	// Initialize trace store (DEBUG mode only)
	traceCfg := tracestore.Config{
		Enabled:        cfg.TraceStore.Enabled,
		RingBufferSize: cfg.TraceStore.RingBufferSize,
		MaxBodySize:    cfg.TraceStore.MaxBodySize,
		DiskPath:       cfg.TraceStore.DiskPath,
		DiskMaxSizeMB:  cfg.TraceStore.DiskMaxSizeMB,
		DiskMaxBackups: cfg.TraceStore.DiskMaxBackups,
		DiskTTLHours:   cfg.TraceStore.DiskTTLHours,
		ChannelBuffer:  cfg.TraceStore.ChannelBuffer,
	}
	// Apply defaults for zero values
	if traceCfg.RingBufferSize == 0 {
		traceCfg.RingBufferSize = 1000
	}
	if traceCfg.MaxBodySize == 0 {
		traceCfg.MaxBodySize = 512 * 1024 // 512KB
	}
	if traceCfg.DiskPath == "" {
		traceCfg.DiskPath = "/var/lib/llms-proxy/trace.log"
	}
	if traceCfg.DiskMaxSizeMB == 0 {
		traceCfg.DiskMaxSizeMB = 500 // 500MB per file
	}
	if traceCfg.DiskMaxBackups == 0 {
		traceCfg.DiskMaxBackups = 10 // 10 files = 5GB total
	}
	if traceCfg.DiskTTLHours == 0 {
		traceCfg.DiskTTLHours = 120 // 5 days
	}
	if traceCfg.ChannelBuffer == 0 {
		traceCfg.ChannelBuffer = 500
	}
	service.traceStore = tracestore.New(traceCfg, logger)

	if err := service.ApplyConfig(cfg); err != nil {
		return nil, err
	}

	return service, nil
}

// UpdateTargets refreshes the known targets from configuration.
func (s *Service) UpdateTargets(targets []config.Target) error {
	parsed, order, err := buildTargetStates(targets, s.logger)
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

	parsed, order, err := buildTargetStates(cfg.Targets, s.logger)
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
		allowed = principal.AllowedTargetsSet()
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

	// Azure-sanitized body is computed lazily — only when the selected target is Azure.
	// This avoids a full JSON unmarshal on every request for non-Azure targets.
	var sanitizedBody []byte
	var strippedFields []string
	sanitizedComputed := false

	var attempted map[string]struct{}
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
		state, selKind, err := s.selectTarget(principal, clientIP(r), requestedLower, allowed, attempted, model, r.URL.Path, time.Now())
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

		if err := s.ensureModelAllowed(target, model); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			s.metrics.totalFailures.Add(1)
			requestOutcomeRecorded = true
			return
		}

		targetKey := strings.ToLower(target.Name)
		if attempted == nil {
			attempted = make(map[string]struct{})
		}
		attempted[targetKey] = struct{}{}

		// Use sanitized body only for Azure OpenAI targets; others get the original.
		forwardBody := bodyBytes
		if target.EndpointType == config.EndpointTypeAzureOpenAI {
			if !sanitizedComputed {
				sanitizedBody, strippedFields = sanitizeRequestBodyForAzure(r, bodyBytes)
				sanitizedComputed = true
				if len(strippedFields) > 0 {
					s.logger.Debug("stripped unsupported request fields",
						"request_id", appmiddleware.RequestIDFromContext(r.Context()),
						"path", r.URL.Path,
						"fields", strings.Join(strippedFields, ","),
					)
				}
			}
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

		// 百炼 Anthropic 格式自动注入 cache_control（仅 3 轮及以上对话）。
		// OpenAI 兼容格式不能注入 Anthropic 专用字段。
		if (target.EndpointType == config.EndpointTypeBailian || target.EndpointType == config.EndpointTypeBailianAPI) &&
			isAnthropicStylePath(r.URL.Path) {
			forwardBody = injectBailianCacheControl(forwardBody)
		}

		startedAt := time.Now()
		var resp *http.Response
		var cancel context.CancelFunc
		var keyIndex int
		var fErr error
		var desperationProbe bool // 标记是否为绝境探测

		// key 池内重试策略：
		// - 429 限流：同 key 退避重试 2 次（指数退避），仍 429 则标记 key 为 rate_limited（30s 冷却），切换到下一个 active key
		// - key 耗尽（quota_exceeded 等）：标记 key 为 exhausted，切换到下一个 active key
		// - 绝境探测：所有 key 都 exhausted 时，选冷却最短的 key 做探测，成功则恢复，失败则延长冷却
		rateLimitRetries := 0
		const maxRateLimitRetries = 2
		convergeCode := ""  // fix-C: 同一请求内连续相同的账户级硬失败码
		convergeStreak := 0 // fix-C: 该码连续命中的不同 key 数
		for {
			resp, cancel, keyIndex, fErr = s.forwardRequest(r, state, forwardBody)
			if fErr != nil {
				break
			}
			if resp == nil || resp.StatusCode < 400 {
				// 请求成功：如果是绝境探测，标记 key 为已恢复
				if desperationProbe && state.keyPool != nil && keyIndex >= 0 {
					state.keyPool.markRecovered(keyIndex)
				}
				break
			}
			// 429 限流：同 key 退避重试，重试 2 次后标记 rate_limited 并换 key
			if resp.StatusCode == 429 && rateLimitRetries < maxRateLimitRetries {
				rateLimitRetries++
				// 指数退避：1s → 2s
				backoff := time.Duration(rateLimitRetries) * time.Second
				if ra := resp.Header.Get("Retry-After"); ra != "" {
					if secs, err := strconv.Atoi(ra); err == nil && secs > 0 && secs <= 10 {
						backoff = time.Duration(secs) * time.Second
					}
				}
				resp.Body.Close()
				if cancel != nil {
					cancel()
				}
				s.logger.Info("[keypool] 429 rate limit, retrying same key",
					"request_id", appmiddleware.RequestIDFromContext(r.Context()),
					"target", target.Name,
					"key_index", keyIndex,
					"retry", rateLimitRetries,
					"backoff_ms", backoff.Milliseconds(),
				)
				s.metrics.totalKeyRetries.Add(1)
				time.Sleep(backoff)
				continue
			}
			// 429 重试 2 次后仍限流：标记 key 为 rate_limited，切换到下一个 active key
			if resp.StatusCode == 429 && rateLimitRetries >= maxRateLimitRetries && state.keyPool != nil && keyIndex >= 0 {
				state.keyPool.markRateLimited(keyIndex)
				resp.Body.Close()
				if cancel != nil {
					cancel()
				}
				nextKey, nextIdx := state.keyPool.selectNextActiveKey(keyIndex)
				if nextKey == "" {
					// 所有 key 都被限流或耗尽，透传 429 给客户端
					s.logger.Warn("[keypool] all keys rate limited or exhausted, passing through 429",
						"request_id", appmiddleware.RequestIDFromContext(r.Context()),
						"target", target.Name,
					)
					// 重新发起请求以获取新的 resp（因为已经 Close 了）
					resp, cancel, keyIndex, fErr = s.forwardRequest(r, state, forwardBody)
					break
				}
				retryClientName := ""
				if principal != nil {
					retryClientName = principal.Name
				}
				s.logger.Info("[keypool] 429 rate limit exhausted, switching to next key",
					"request_id", appmiddleware.RequestIDFromContext(r.Context()),
					"target", target.Name,
					"client", retryClientName,
					"prev_key_index", keyIndex,
					"next_key_index", nextIdx,
				)
				s.metrics.totalKeyRetries.Add(1)
				rateLimitRetries = 0 // 重置重试计数
				continue
			}
			if state.keyPool == nil || keyIndex < 0 {
				break
			}
			// fix-C 收敛：同一请求内，若连续 ≥2 个不同 key 返回相同的"账户级硬失败"码
			// （invalid_token / billing_error / account_disabled / 总额度耗尽等），说明换 key
			// 无望（同账户共享失效），停止逐 key 重试，直接把真实上游响应透传给客户端——
			// 既避免把整个池全部标记耗尽 + 触发唤醒风暴，也让客户端看到真实的 401 而非笼统 503。
			if reason := state.keyPool.exhaustReasonAt(keyIndex); isAccountLevelExhaustion(reason) {
				if reason == convergeCode {
					convergeStreak++
				} else {
					convergeCode = reason
					convergeStreak = 1
				}
				if convergeStreak >= 2 {
					s.logger.Warn("[keypool] convergence: multiple keys returned same account-level failure, passing through upstream response",
						"request_id", appmiddleware.RequestIDFromContext(r.Context()),
						"target", target.Name,
						"reason", reason,
						"keys_tried", convergeStreak,
					)
					break // 保持 resp 打开 → 交由 writeResponse 透传真实上游响应
				}
			}
			// 检查是否有下一个可用 key（当前 key 已在 forwardRequest 中被标记耗尽）
			nextKey, nextIdx := state.keyPool.selectKey()
			if nextKey == "" || nextIdx == keyIndex {
				break
			}
			// 判断是否为绝境探测：nextIdx 对应的 key 仍处于 exhausted 状态
			state.keyPool.mu.Lock()
			if nextIdx >= 0 && nextIdx < len(state.keyPool.entries) && state.keyPool.entries[nextIdx].exhausted {
				desperationProbe = true
			} else {
				desperationProbe = false
			}
			state.keyPool.mu.Unlock()
			// 有新 key 可用，关闭当前响应，重试
			resp.Body.Close()
			if cancel != nil {
				cancel()
			}
			retryClientName := ""
			if principal != nil {
				retryClientName = principal.Name
			}
			s.logger.Info("[keypool] retrying with next key",
				"request_id", appmiddleware.RequestIDFromContext(r.Context()),
				"target", target.Name,
				"client", retryClientName,
				"prev_key_index", keyIndex,
				"next_key_index", nextIdx,
			)
			s.metrics.totalKeyRetries.Add(1)
		}

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

			// 旁路：上游网络错误 / 配置错误 → upstream-error.log
			if ok {
				writeForwardErrorLog(r, state, fe)
			}

			if ok && fe.retryable {
				s.handleForwardError(r, state, fe.err, status)
				if !allowFallback {
					http.Error(w, http.StatusText(status), status)
					s.metrics.totalFailures.Add(1)
					requestOutcomeRecorded = true
					return
				}

				s.metrics.totalTargetRetries.Add(1)
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

		s.writeResponse(w, r, state, resp, cancel, attempt, model, forwardBody, startedAt, keyIndex)
		// 更新连接粘连：仅在粘连命中（刷新 TTL）或首次轮询（建立粘连）时更新。
		// Failover（原粘连目标暂不可用）和显式指定目标时不更新，避免劫持粘连。
		if principal != nil && target != nil && (selKind == selectionAffinityHit || selKind == selectionRoundRobin) {
			s.affinity.Set(affinityKey(clientIP(r), principal.Name, model), strings.ToLower(target.Name), time.Now())
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
	keyRetries := s.metrics.totalKeyRetries.Load()
	targetRetries := s.metrics.totalTargetRetries.Load()
	return ServiceMetrics{
		TotalRequests:      s.metrics.totalRequests.Load(),
		TotalSuccess:       s.metrics.totalSuccess.Load(),
		TotalFailures:      s.metrics.totalFailures.Load(),
		TotalRetries:       keyRetries + targetRetries,
		TotalKeyRetries:    keyRetries,
		TotalTargetRetries: targetRetries,
		ActiveRequests:     s.metrics.activeRequests.Load(),
		StartTime:          s.startTime,
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

		// 统计 key pool 状态
		var totalKeys, activeKeys, exhaustedSubKeys, exhaustedAPIKeys, rateLimitedKeys, blockedKeys, otherExhaustedKeys int
		if state.keyPool != nil {
			keyStatuses := state.keyPool.status()
			totalKeys = len(keyStatuses)
			for _, ks := range keyStatuses {
				switch {
				case ks.Blocked:
					blockedKeys++
				case !ks.Exhausted:
					activeKeys++
				case ks.ExhaustReason == "quota_exceeded_subscription":
					exhaustedSubKeys++
				case ks.ExhaustReason == "quota_exceeded_api":
					exhaustedAPIKeys++
				case ks.ExhaustReason == "rate_limited":
					rateLimitedKeys++
				default:
					// 其他失效原因：invalid_token / billing_error /
					// account_disabled / 旧版 quota_exceeded 等，均计入此处
					// 以保证各分类之和等于 totalKeys。
					otherExhaustedKeys++
				}
			}
		} else if target.APIKey != "" {
			// 单 key 模式
			totalKeys = 1
			activeKeys = 1
		}

		statuses = append(statuses, TargetStatus{
			Name:                      target.Name,
			EndpointType:              target.EndpointType,
			Endpoint:                  endpoint,
			ResourcePathPrefix:        target.ResourcePathPrefix,
			Muted:                     state.IsMuted(now),
			MutedUntil:                stats.MutedUntil,
			LastSuccess:               stats.LastSuccess,
			LastFailure:               stats.LastFailure,
			ConsecutiveFailures:       stats.ConsecutiveFailure,
			TotalSuccessRequests:      stats.TotalSuccess,
			TotalFailedRequests:       stats.TotalFailure,
			TotalKeys:                 totalKeys,
			ActiveKeys:                activeKeys,
			ExhaustedSubscriptionKeys: exhaustedSubKeys,
			ExhaustedAPIKeys:          exhaustedAPIKeys,
			RateLimitedKeys:           rateLimitedKeys,
			BlockedKeys:               blockedKeys,
			OtherExhaustedKeys:        otherExhaustedKeys,
		})
	}
	return statuses
}

// StartTime returns the instant the service was constructed.
func (s *Service) StartTime() time.Time {
	return s.startTime
}

// KeyPoolStatus returns the key pool status for a target, or nil if no pool.
func (s *Service) KeyPoolStatus(targetName string) []KeyStatus {
	s.mu.RLock()
	state, ok := s.targetsByName[strings.ToLower(targetName)]
	s.mu.RUnlock()
	if !ok || state == nil || state.keyPool == nil {
		return nil
	}
	return state.keyPool.status()
}

// BlockKey manually blocks a specific key in the target's key pool.
func (s *Service) BlockKey(targetName string, index int) error {
	s.mu.RLock()
	state, ok := s.targetsByName[strings.ToLower(targetName)]
	s.mu.RUnlock()
	if !ok || state == nil || state.keyPool == nil {
		return fmt.Errorf("target %q not found or has no key pool", targetName)
	}
	if index < 0 || index >= len(state.keyPool.entries) {
		return fmt.Errorf("key index %d out of range", index)
	}
	state.keyPool.blockKey(index)
	return nil
}

// UnblockKey manually unblocks a specific key in the target's key pool.
func (s *Service) UnblockKey(targetName string, index int) error {
	s.mu.RLock()
	state, ok := s.targetsByName[strings.ToLower(targetName)]
	s.mu.RUnlock()
	if !ok || state == nil || state.keyPool == nil {
		return fmt.Errorf("target %q not found or has no key pool", targetName)
	}
	if index < 0 || index >= len(state.keyPool.entries) {
		return fmt.Errorf("key index %d out of range", index)
	}
	state.keyPool.unblockKey(index)
	return nil
}

// WakeUpKeys triggers the wake-up model for a target's key pool.
// Returns the number of keys recovered, or an error if the target has no key pool.
func (s *Service) WakeUpKeys(targetName string) (recovered int, err error) {
	s.mu.RLock()
	state, ok := s.targetsByName[strings.ToLower(targetName)]
	s.mu.RUnlock()
	if !ok || state == nil || state.keyPool == nil {
		return 0, fmt.Errorf("target %q not found or has no key pool", targetName)
	}

	now := time.Now()
	if !state.keyPool.tryWakeUp(now) {
		return 0, fmt.Errorf("wake-up already in progress or in cooldown")
	}
	defer state.keyPool.wakeUpComplete()

	// 对每个 exhausted/blocked 的 key 发送轻量探测请求，仅在探测成功
	// 且确实由 exhausted 转回 active 时才计入恢复数，避免计数虚高。
	ctx := context.Background()
	exhaustedKeys := state.keyPool.getExhaustedKeys()
	for _, idx := range exhaustedKeys {
		if !s.probeKey(ctx, state, idx) {
			continue // 探测失败，key 仍无效，保持 exhausted
		}
		if state.keyPool.markRecovered(idx) {
			recovered++
		}
	}

	return recovered, nil
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
		return 1800 * time.Second
	}
	return timeout
}

// GetTrace 按 request_id 查询单条 trace 记录（DEBUG 模式）。
func (s *Service) GetTrace(requestID string) *tracestore.TraceRecord {
	if s.traceStore == nil {
		return nil
	}
	return s.traceStore.Get(requestID)
}

// ListTrace 列出最近的 trace 记录（DEBUG 模式）。
func (s *Service) ListTrace(limit int) []*tracestore.TraceRecord {
	if s.traceStore == nil {
		return nil
	}
	return s.traceStore.List(limit)
}

// TraceStats 返回 trace store 的统计信息（DEBUG 模式）。
func (s *Service) TraceStats() map[string]int64 {
	if s.traceStore == nil {
		return nil
	}
	return s.traceStore.Stats()
}
