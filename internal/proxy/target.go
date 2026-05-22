// target.go — 目标选择（round-robin + muting）与健康状态管理。
package proxy

import (
	"fmt"
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

func (s *Service) selectTarget(
	principal *auth.Principal,
	requestedLower string,
	allowed map[string]struct{},
	attempted map[string]struct{},
	model string,
	path string,
	endpointTypeHint string,
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
		if !PathSupportedByEndpointType(state.Target().EndpointType, path) {
			return nil, newSelectionError(http.StatusBadRequest, fmt.Sprintf("target %q does not support path %q", state.Target().Name, path))
		}
		if endpointTypeHint != "" && state.Target().EndpointType != endpointTypeHint {
			return nil, newSelectionError(http.StatusBadRequest, fmt.Sprintf("target %q endpoint_type %q does not match required %q", state.Target().Name, state.Target().EndpointType, endpointTypeHint))
		}
		if _, tried := attempted[strings.ToLower(state.Target().Name)]; tried {
			return nil, newSelectionError(http.StatusServiceUnavailable, "requested target already attempted")
		}
		if state.IsMuted(now) {
			return nil, newSelectionError(http.StatusServiceUnavailable, "requested target temporarily unavailable")
		}
		return state, nil
	}

	// 连接粘连：优先复用上次成功路由的目标
	if principal != nil && len(attempted) == 0 {
		aKey := affinityKey(principal.Name, model)
		if affinityTarget, ok := s.affinity.Get(aKey, now); ok {
			if state, exists := s.targetByName(affinityTarget); exists {
				t := state.Target()
				nameKey := strings.ToLower(t.Name)
				allowedOK := allowed == nil
				if !allowedOK {
					_, allowedOK = allowed[nameKey]
				}
				hintOK := endpointTypeHint == "" || t.EndpointType == endpointTypeHint
				if t != nil && allowedOK && hintOK && !state.IsMuted(now) && modelAllowed(t, model) && PathSupportedByEndpointType(t.EndpointType, path) {
					return state, nil
				}
			}
		}
	}

	candidate, mutedCandidate := s.findAvailableTargetWithModel(allowed, attempted, model, path, endpointTypeHint, now)
	if candidate != nil {
		return candidate, nil
	}

	if mutedCandidate != nil {
		return nil, newSelectionError(http.StatusServiceUnavailable, fmt.Sprintf("all targets muted until %s", mutedCandidate.NextRetry().Format(time.RFC3339)))
	}

	if len(attempted) > 0 {
		return nil, newSelectionError(http.StatusBadGateway, "no alternative target available")
	}

	if endpointTypeHint != "" {
		return nil, newSelectionError(http.StatusBadGateway, fmt.Sprintf("no target available for endpoint_type %q", endpointTypeHint))
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

func (s *Service) findAvailableTarget(allowed map[string]struct{}, attempted map[string]struct{}, now time.Time) (*targetState, *targetState) {
	return s.findAvailableTargetWithModel(allowed, attempted, "", "", "", now)
}

func (s *Service) findAvailableTargetWithModel(allowed map[string]struct{}, attempted map[string]struct{}, model string, path string, endpointTypeHint string, now time.Time) (*targetState, *targetState) {
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
		if endpointTypeHint != "" && state.Target().EndpointType != endpointTypeHint {
			continue
		}
		if !modelAllowed(state.Target(), model) {
			continue
		}
		if !PathSupportedByEndpointType(state.Target().EndpointType, path) {
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

func buildTargetStates(targets []config.Target) (map[string]*targetState, []*targetState, error) {
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
			APIKey:             strings.TrimSpace(t.APIKey),
		AllowBearer:        t.AllowBearer,
		AuthMode:           t.AuthMode,
		AllowedModels:      models,
			SSEAutoAggregate:   t.SSEAutoAggregate == nil || *t.SSEAutoAggregate,
			allowedModelsSet:   modelSet,
		}
		if info.Name == "" {
			return nil, nil, fmt.Errorf("proxy: target name at index %d must not be empty", idx)
		}
		if info.APIKey == "" && !info.AllowBearer {
			return nil, nil, fmt.Errorf("proxy: target %q missing api_key and bearer passthrough not enabled", info.Name)
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
