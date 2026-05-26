package auth

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/ycgame/llms-proxy/internal/config"
)

// Principal represents an authenticated client.
type Principal struct {
	Name           string
	AccessKey      string // 客户端的 access_key（唯一标识）
	allowAll       bool
	allowedTargets map[string]struct{}
	allowedList    []string
}

// AllowedTargets returns a copy of the allowed targets list.
func (p *Principal) AllowedTargets() []string {
	cloned := make([]string, len(p.allowedList))
	copy(cloned, p.allowedList)
	return cloned
}

// AllowedTargetsSet returns the normalized allowed targets set directly.
// The returned map must NOT be modified by the caller.
// This avoids the allocation overhead of AllowedTargets() + normalizeAllowed().
func (p *Principal) AllowedTargetsSet() map[string]struct{} {
	return p.allowedTargets
}

// AllowAll indicates whether the principal can access all targets.
func (p *Principal) AllowAll() bool {
	return p.allowAll
}

// CanAccess checks if the principal can access the provided target.
func (p *Principal) CanAccess(target string) bool {
	if p == nil {
		return false
	}
	if p.allowAll {
		return true
	}
	_, ok := p.allowedTargets[strings.ToLower(target)]
	return ok
}

// Clone returns a deep copy of the principal.
func (p *Principal) Clone() *Principal {
	if p == nil {
		return nil
	}
	clone := &Principal{
		Name:        p.Name,
		AccessKey:   p.AccessKey,
		allowAll:    p.allowAll,
		allowedList: make([]string, len(p.allowedList)),
	}
	copy(clone.allowedList, p.allowedList)
	if len(p.allowedTargets) > 0 {
		clone.allowedTargets = make(map[string]struct{}, len(p.allowedTargets))
		for k, v := range p.allowedTargets {
			clone.allowedTargets[k] = v
		}
	} else {
		clone.allowedTargets = make(map[string]struct{})
	}
	return clone
}

// Store keeps runtime authentication data.
type Store struct {
	mu      sync.RWMutex
	clients map[string]*Principal
}

// NewStore constructs an empty Store.
func NewStore() *Store {
	return &Store{
		clients: make(map[string]*Principal),
	}
}

// LoadFromConfig replaces the current client map from configuration.
func (s *Store) LoadFromConfig(clients []config.Client) error {
	if s == nil {
		return errors.New("auth store is nil")
	}
	next := make(map[string]*Principal, len(clients))
	owners := make(map[string]string, len(clients))
	for _, client := range clients {
		key := strings.TrimSpace(client.AccessKey)
		if key == "" {
			return fmt.Errorf("client %q missing access_key", client.Name)
		}
		name := strings.TrimSpace(client.Name)
		if name == "" {
			return errors.New("client name must not be empty")
		}
		if prevOwner, exists := owners[key]; exists {
			return fmt.Errorf("duplicate access_key for clients %q and %q", prevOwner, name)
		}
		owners[key] = name

		principal := &Principal{
			Name:           name,
			AccessKey:      key,
			allowAll:       len(client.AllowedTargets) == 0,
			allowedTargets: make(map[string]struct{}),
			allowedList:    make([]string, 0, len(client.AllowedTargets)),
		}

		for _, target := range client.AllowedTargets {
			target = strings.ToLower(strings.TrimSpace(target))
			if target == "" {
				continue
			}
			if _, exists := principal.allowedTargets[target]; !exists {
				principal.allowedTargets[target] = struct{}{}
				principal.allowedList = append(principal.allowedList, target)
			}
		}

		next[key] = principal
	}

	s.mu.Lock()
	s.clients = next
	s.mu.Unlock()
	return nil
}

// Authenticate returns the principal associated with the key.
func (s *Store) Authenticate(key string) (*Principal, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	principal, ok := s.clients[key]
	if !ok {
		return nil, false
	}
	return principal.Clone(), true
}
