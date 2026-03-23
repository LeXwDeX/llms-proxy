package nosql

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/ycgame/llms-proxy/internal/config"
)

// ClientStore manages clients backed by a JSON file.
type ClientStore struct {
	mu   sync.RWMutex
	path string
}

// NewClientStore creates a new file-backed client store.
func NewClientStore(path string) *ClientStore {
	return &ClientStore{path: strings.TrimSpace(path)}
}

// Path returns current clients file path.
func (s *ClientStore) Path() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.path
}

// SetPath updates clients file path.
func (s *ClientStore) SetPath(path string) {
	s.mu.Lock()
	s.path = strings.TrimSpace(path)
	s.mu.Unlock()
}

// List returns all clients from store.
func (s *ClientStore) List() ([]config.Client, error) {
	s.mu.RLock()
	path := s.path
	s.mu.RUnlock()

	clients, err := readClients(path)
	if err != nil {
		return nil, err
	}
	return cloneClients(clients), nil
}

// Create appends a new client. It fails when name/access_key already exists.
func (s *ClientStore) Create(client config.Client) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := s.path
	clients, err := readClients(path)
	if err != nil {
		return err
	}

	if err := validateClient(client); err != nil {
		return err
	}
	if hasClientName(clients, client.Name) {
		return fmt.Errorf("client %q already exists", strings.TrimSpace(client.Name))
	}
	if hasClientAccessKey(clients, client.AccessKey) {
		return errors.New("access_key already exists")
	}

	client = normalizeClient(client)
	clients = append(clients, client)
	return writeClients(path, clients)
}

// Update updates one client by name.
func (s *ClientStore) Update(name string, client config.Client) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := s.path
	clients, err := readClients(path)
	if err != nil {
		return err
	}

	if err := validateClient(client); err != nil {
		return err
	}

	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("name must not be empty")
	}

	idx := -1
	for i := range clients {
		if strings.EqualFold(strings.TrimSpace(clients[i].Name), name) {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("client %q not found", name)
	}

	next := normalizeClient(client)
	for i := range clients {
		if i == idx {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(clients[i].Name), next.Name) {
			return fmt.Errorf("client %q already exists", next.Name)
		}
		if strings.TrimSpace(clients[i].AccessKey) == next.AccessKey {
			return errors.New("access_key already exists")
		}
	}

	clients[idx] = next
	return writeClients(path, clients)
}

// Delete removes one client by name.
func (s *ClientStore) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := s.path
	clients, err := readClients(path)
	if err != nil {
		return err
	}

	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("name must not be empty")
	}

	next := make([]config.Client, 0, len(clients))
	removed := false
	for _, item := range clients {
		if strings.EqualFold(strings.TrimSpace(item.Name), name) {
			removed = true
			continue
		}
		next = append(next, normalizeClient(item))
	}

	if !removed {
		return fmt.Errorf("client %q not found", name)
	}

	return writeClients(path, next)
}

func readClients(path string) ([]config.Client, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("clients file path is empty")
	}

	if err := ensureJSONFile(path, []byte("[]\n")); err != nil {
		return nil, err
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read clients file: %w", err)
	}

	var clients []config.Client
	if len(strings.TrimSpace(string(content))) == 0 {
		return []config.Client{}, nil
	}
	if err := json.Unmarshal(content, &clients); err != nil {
		return nil, fmt.Errorf("decode clients file: %w", err)
	}

	for i := range clients {
		if err := validateClient(clients[i]); err != nil {
			return nil, fmt.Errorf("clients[%d]: %w", i, err)
		}
		clients[i] = normalizeClient(clients[i])
	}

	for i := range clients {
		for j := i + 1; j < len(clients); j++ {
			if strings.EqualFold(clients[i].Name, clients[j].Name) {
				return nil, fmt.Errorf("duplicate client name %q", clients[i].Name)
			}
			if clients[i].AccessKey == clients[j].AccessKey {
				return nil, fmt.Errorf("duplicate access_key for clients %q and %q", clients[i].Name, clients[j].Name)
			}
		}
	}

	return clients, nil
}

func writeClients(path string, clients []config.Client) error {
	normalized := cloneClients(clients)
	for i := range normalized {
		normalized[i] = normalizeClient(normalized[i])
	}

	payload, err := json.MarshalIndent(normalized, "", "  ")
	if err != nil {
		return fmt.Errorf("encode clients file: %w", err)
	}
	payload = append(payload, '\n')

	if err := writeAtomic(path, payload); err != nil {
		return fmt.Errorf("write clients file: %w", err)
	}
	return nil
}

func hasClientName(clients []config.Client, name string) bool {
	for _, item := range clients {
		if strings.EqualFold(strings.TrimSpace(item.Name), strings.TrimSpace(name)) {
			return true
		}
	}
	return false
}

func hasClientAccessKey(clients []config.Client, key string) bool {
	for _, item := range clients {
		if strings.TrimSpace(item.AccessKey) == strings.TrimSpace(key) {
			return true
		}
	}
	return false
}

func normalizeClient(client config.Client) config.Client {
	client.Name = strings.TrimSpace(client.Name)
	client.AccessKey = strings.TrimSpace(client.AccessKey)
	normalizedTargets := make([]string, 0, len(client.AllowedTargets))
	seen := make(map[string]struct{}, len(client.AllowedTargets))
	for _, target := range client.AllowedTargets {
		target = strings.ToLower(strings.TrimSpace(target))
		if target == "" {
			continue
		}
		if _, ok := seen[target]; ok {
			continue
		}
		seen[target] = struct{}{}
		normalizedTargets = append(normalizedTargets, target)
	}
	client.AllowedTargets = normalizedTargets
	return client
}

func validateClient(client config.Client) error {
	if strings.TrimSpace(client.Name) == "" {
		return errors.New("name must not be empty")
	}
	if strings.TrimSpace(client.AccessKey) == "" {
		return errors.New("access_key must not be empty")
	}
	return nil
}

func cloneClients(clients []config.Client) []config.Client {
	cloned := make([]config.Client, len(clients))
	for i := range clients {
		cloned[i] = clients[i]
		if len(clients[i].AllowedTargets) > 0 {
			cloned[i].AllowedTargets = append([]string(nil), clients[i].AllowedTargets...)
		}
	}
	return cloned
}

func ensureJSONFile(path string, initial []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create parent dir: %w", err)
	}
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.WriteFile(path, initial, 0o644); err != nil {
		return fmt.Errorf("create json file: %w", err)
	}
	return nil
}
