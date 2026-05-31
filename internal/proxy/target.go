// target.go — 目标选择（round-robin + muting）与健康状态管理。
package proxy

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ycgame/llms-proxy/internal/auth"
	"github.com/ycgame/llms-proxy/internal/config"
)

type targetState struct {
	target *Target

	mu                 sync.RWMutex
	mutedUntil         time.Time
	lastSuccess        time.Time
	lastFailure        time.Time
	consecutiveFailure int
	totalSuccess       int64
	totalFailure       int64

	keyPool *keyPool
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

func newTargetState(t *Target, logger *slog.Logger) *targetState {
	s := &targetState{target: t}
	if len(t.APIKeys) > 1 {
		s.keyPool = newKeyPool(t.Name, t.APIKeys, t.KeyResetTime, logger)
	}
	return s
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

// selectionKind 描述目标选择的路径，用于决定是否更新连接粘连。
type selectionKind int

const (
	selectionExplicit    selectionKind = iota // 用户显式指定目标（header/query）
	selectionAffinityHit                      // 粘连命中，目标健康可用
	selectionRoundRobin                       // 无粘连记录，轮询选出（首次或过期后）
	selectionFailover                         // 有粘连记录但目标不可用，failover 到其他目标
)

func (s *Service) selectTarget(
	principal *auth.Principal,
	clientIP string,
	requestedLower string,
	allowed map[string]struct{},
	attempted map[string]struct{},
	model string,
	path string,
	now time.Time,
) (*targetState, selectionKind, error) {
	if requestedLower != "" {
		if !principal.CanAccess(requestedLower) {
			return nil, selectionExplicit, newSelectionError(http.StatusForbidden, "target not allowed")
		}
		state, ok := s.targetByName(requestedLower)
		if !ok {
			return nil, selectionExplicit, newSelectionError(http.StatusBadRequest, "unknown target")
		}
		if !modelAllowed(state.Target(), model) {
			return nil, selectionExplicit, newSelectionError(http.StatusBadRequest, fmt.Sprintf("model %q not allowed for target %q", model, state.Target().Name))
		}
		if state.Target().Paused {
			return nil, selectionExplicit, newSelectionError(http.StatusServiceUnavailable, "requested target paused")
		}
		if !state.Target().SupportsPath(path) {
			return nil, selectionExplicit, newSelectionError(http.StatusBadRequest, fmt.Sprintf("target %q does not support path %q", state.Target().Name, path))
		}
		if _, tried := attempted[strings.ToLower(state.Target().Name)]; tried {
			return nil, selectionExplicit, newSelectionError(http.StatusServiceUnavailable, "requested target already attempted")
		}
		if state.IsMuted(now) {
			return nil, selectionExplicit, newSelectionError(http.StatusServiceUnavailable, "requested target temporarily unavailable")
		}
		return state, selectionExplicit, nil
	}

	// 连接粘连：优先复用上次成功路由的目标
	hadAffinity := false
	if principal != nil && len(attempted) == 0 {
		aKey := affinityKey(clientIP, principal.Name, model)
		if affinityTarget, ok := s.affinity.Get(aKey, now); ok {
			hadAffinity = true
			if state, exists := s.targetByName(affinityTarget); exists {
				t := state.Target()
				nameKey := strings.ToLower(t.Name)
				allowedOK := allowed == nil
				if !allowedOK {
					_, allowedOK = allowed[nameKey]
				}
				if t != nil && allowedOK && !t.Paused && !state.IsMuted(now) && modelAllowed(t, model) && t.SupportsPath(path) {
					return state, selectionAffinityHit, nil
				}
			}
		}
	}

	candidate, mutedCandidate := s.findAvailableTargetWithModel(allowed, attempted, model, path, now)
	if candidate != nil {
		// 如果之前有粘连记录但目标不可用，这是 failover；否则是首次轮询。
		kind := selectionRoundRobin
		if hadAffinity {
			kind = selectionFailover
		}
		return candidate, kind, nil
	}

	if mutedCandidate != nil {
		return nil, selectionRoundRobin, newSelectionError(http.StatusServiceUnavailable, fmt.Sprintf("all targets muted until %s", mutedCandidate.NextRetry().Format(time.RFC3339)))
	}

	if len(attempted) > 0 {
		return nil, selectionRoundRobin, newSelectionError(http.StatusBadGateway, "no alternative target available")
	}

	if model != "" {
		if s.hasModelCandidateIgnoringPath(allowed, attempted, model) {
			return nil, selectionRoundRobin, newSelectionError(http.StatusBadRequest, fmt.Sprintf("no target supports path %q for model %q", path, model))
		}
		return nil, selectionRoundRobin, newSelectionError(http.StatusBadRequest, "model not supported by any target")
	}

	return nil, selectionRoundRobin, newSelectionError(http.StatusBadGateway, "no target available")
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

func (s *Service) findAvailableTarget(allowed map[string]struct{}, attempted map[string]struct{}, now time.Time) (*targetState, *targetState) {
	return s.findAvailableTargetWithModel(allowed, attempted, "", "", now)
}

func (s *Service) hasModelCandidateIgnoringPath(allowed map[string]struct{}, attempted map[string]struct{}, model string) bool {
	for _, state := range s.targetSnapshot() {
		if state == nil || state.Target() == nil {
			continue
		}
		target := state.Target()
		if target.Paused {
			continue
		}
		name := strings.ToLower(target.Name)
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
		if modelAllowed(target, model) {
			return true
		}
	}
	return false
}

func (s *Service) findAvailableTargetWithModel(allowed map[string]struct{}, attempted map[string]struct{}, model string, path string, now time.Time) (*targetState, *targetState) {
	var mutedCandidate *targetState
	snapshot := s.targetSnapshot()
	if len(snapshot) == 0 {
		return nil, nil
	}

	// 第一步：过滤出支持该模型的 targets，并统计 key 权重
	// #1 加权改用「当前存活 key 数」：冷却/耗尽的 key 不再吸引流量，
	// 避免请求被分配到实际无可用 key 的 target 后再触发重试。
	type targetWithKeys struct {
		state       *targetState
		keyCount    int // 配置 key 数（兜底权重）
		activeCount int // 当前存活 key 数（首选权重）
	}
	var candidates []targetWithKeys
	totalKeys := 0
	totalActive := 0

	for _, state := range snapshot {
		if state == nil || state.Target() == nil {
			continue
		}
		target := state.Target()
		if target.Paused {
			continue
		}
		name := strings.ToLower(target.Name)
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
		if !modelAllowed(target, model) {
			continue
		}
		if !target.SupportsPath(path) {
			continue
		}

		// 统计该 target 的配置 key 数量（兜底）
		keyCount := 1 // 至少有 api_key
		if len(target.APIKeys) > 0 {
			keyCount = len(target.APIKeys)
		}
		// 统计当前存活 key 数量（首选权重）。
		// 无 keyPool（单 key target）时退化为配置数，避免被多 key target 的存活权重挤占到 0。
		activeCount := keyCount
		if state.keyPool != nil {
			activeCount = state.keyPool.activeKeyCount()
		}

		candidates = append(candidates, targetWithKeys{state: state, keyCount: keyCount, activeCount: activeCount})
		totalKeys += keyCount
		totalActive += activeCount
	}

	if len(candidates) == 0 {
		return nil, nil
	}

	// 选择权重来源：只要存在任一存活 key，就按存活数加权；
	// 否则（全部 target 当前无存活 key）回退到配置 key 数加权，保证仍能选出 target 去探测恢复。
	weightOf := func(c targetWithKeys) int { return c.keyCount }
	totalWeight := totalKeys
	if totalActive > 0 {
		weightOf = func(c targetWithKeys) int { return c.activeCount }
		totalWeight = totalActive
	}

	// 第二步：按 key 权重做 round-robin（每个存活 key 有相等概率）
	// 例如：target-A 有 4 个存活 key，target-B 有 1 个
	// 总权重 = 5，轮询 5 次为一个周期
	// target-A 获得 4/5 = 80% 流量，target-B 获得 1/5 = 20% 流量
	keyIndex := int(s.rrCounter.Add(1)-1) % totalWeight
	cumulativeKeys := 0

	for _, candidate := range candidates {
		cumulativeKeys += weightOf(candidate)
		if keyIndex < cumulativeKeys {
			if !candidate.state.IsMuted(now) {
				return candidate.state, mutedCandidate
			}
			if mutedCandidate == nil || candidate.state.NextRetry().Before(mutedCandidate.NextRetry()) {
				mutedCandidate = candidate.state
			}
			break
		}
	}

	// 如果选中的 target 被 muted，尝试其他 candidates
	for _, candidate := range candidates {
		if !candidate.state.IsMuted(now) {
			return candidate.state, mutedCandidate
		}
		if mutedCandidate == nil || candidate.state.NextRetry().Before(mutedCandidate.NextRetry()) {
			mutedCandidate = candidate.state
		}
	}

	return nil, mutedCandidate
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

func buildTargetStates(targets []config.Target, logger ...*slog.Logger) (map[string]*targetState, []*targetState, error) {
	var l *slog.Logger
	if len(logger) > 0 {
		l = logger[0]
	}
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

		// 合并 api_key + api_keys 为有序池（去重，api_key 排第一）
		primaryKey := strings.TrimSpace(t.APIKey)
		var mergedKeys []string
		seen := make(map[string]struct{})
		if primaryKey != "" {
			mergedKeys = append(mergedKeys, primaryKey)
			seen[primaryKey] = struct{}{}
		}
		for _, k := range t.APIKeys {
			k = strings.TrimSpace(k)
			if k == "" {
				continue
			}
			if _, dup := seen[k]; dup {
				continue
			}
			mergedKeys = append(mergedKeys, k)
			seen[k] = struct{}{}
		}

		info := &Target{
			Name:               strings.TrimSpace(t.Name),
			EndpointType:       config.NormalizeEndpointType(t.EndpointType),
			Endpoint:           endpoint,
			ResourcePathPrefix: normalizePrefix(t.ResourcePathPrefix),
			APIKey:             primaryKey,
			APIKeys:            mergedKeys,
			KeyResetTime:       t.KeyResetTime,
			Paused:             t.Paused,
			AllowBearer:        t.AllowBearer,
			AuthMode:           t.AuthMode,
			AllowedModels:      models,
			SSEAutoAggregate:   t.SSEAutoAggregate == nil || *t.SSEAutoAggregate,
			OpenAIPrefix:       t.OpenAIPrefix,
			AnthropicPrefix:    t.AnthropicPrefix,
			SupportsResponses:  t.SupportsResponses,
			allowedModelsSet:   modelSet,
		}
		if info.Name == "" {
			return nil, nil, fmt.Errorf("proxy: target name at index %d must not be empty", idx)
		}
		if len(info.APIKeys) == 0 && !info.AllowBearer {
			return nil, nil, fmt.Errorf("proxy: target %q missing api_key and bearer passthrough not enabled", info.Name)
		}

		nameKey := strings.ToLower(info.Name)
		if _, exists := parsed[nameKey]; exists {
			return nil, nil, fmt.Errorf("proxy: duplicate target name %q", info.Name)
		}

		state := newTargetState(info, l)
		parsed[nameKey] = state
		order = append(order, state)
	}

	return parsed, order, nil
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
