package nosql

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// ModelCost defines token pricing (per 1k tokens).
type ModelCost struct {
	Model                 string  `json:"model"`
	InputPer1KTokens      float64 `json:"input_per_1k_tokens"`
	OutputPer1KTokens     float64 `json:"output_per_1k_tokens"`
	CachedInputPer1KToken float64 `json:"cached_input_per_1k_tokens"`
}

// ModelCostStore manages model costs backed by JSON file.
type ModelCostStore struct {
	mu   sync.RWMutex
	path string
}

// NewModelCostStore creates a new model cost store.
func NewModelCostStore(path string) *ModelCostStore {
	return &ModelCostStore{path: strings.TrimSpace(path)}
}

// Path returns current file path.
func (s *ModelCostStore) Path() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.path
}

// SetPath updates file path.
func (s *ModelCostStore) SetPath(path string) {
	s.mu.Lock()
	s.path = strings.TrimSpace(path)
	s.mu.Unlock()
}

// List returns all model costs.
func (s *ModelCostStore) List() ([]ModelCost, error) {
	s.mu.RLock()
	path := s.path
	s.mu.RUnlock()

	costs, err := readModelCosts(path)
	if err != nil {
		return nil, err
	}
	return cloneCosts(costs), nil
}

// Upsert inserts or updates one model cost.
func (s *ModelCostStore) Upsert(cost ModelCost) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := validateCost(cost); err != nil {
		return err
	}
	cost = normalizeCost(cost)

	path := s.path
	costs, err := readModelCosts(path)
	if err != nil {
		return err
	}

	replaced := false
	for i := range costs {
		if strings.EqualFold(costs[i].Model, cost.Model) {
			costs[i] = cost
			replaced = true
			break
		}
	}
	if !replaced {
		costs = append(costs, cost)
	}

	return writeModelCosts(path, costs)
}

// Delete removes one model cost by model name.
func (s *ModelCostStore) Delete(model string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	model = strings.TrimSpace(model)
	if model == "" {
		return errors.New("model must not be empty")
	}

	path := s.path
	costs, err := readModelCosts(path)
	if err != nil {
		return err
	}

	next := make([]ModelCost, 0, len(costs))
	removed := false
	for _, item := range costs {
		if strings.EqualFold(item.Model, model) {
			removed = true
			continue
		}
		next = append(next, item)
	}
	if !removed {
		return fmt.Errorf("model %q not found", model)
	}

	return writeModelCosts(path, next)
}

func readModelCosts(path string) ([]ModelCost, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("model costs file path is empty")
	}

	if err := ensureJSONFile(path, []byte("[]\n")); err != nil {
		return nil, err
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read model costs file: %w", err)
	}
	if len(strings.TrimSpace(string(content))) == 0 {
		return []ModelCost{}, nil
	}

	var costs []ModelCost
	if err := json.Unmarshal(content, &costs); err != nil {
		return nil, fmt.Errorf("decode model costs file: %w", err)
	}

	for i := range costs {
		if err := validateCost(costs[i]); err != nil {
			return nil, fmt.Errorf("model_costs[%d]: %w", i, err)
		}
		costs[i] = normalizeCost(costs[i])
	}

	for i := range costs {
		for j := i + 1; j < len(costs); j++ {
			if strings.EqualFold(costs[i].Model, costs[j].Model) {
				return nil, fmt.Errorf("duplicate model %q", costs[i].Model)
			}
		}
	}

	return costs, nil
}

func writeModelCosts(path string, costs []ModelCost) error {
	normalized := cloneCosts(costs)
	for i := range normalized {
		normalized[i] = normalizeCost(normalized[i])
	}

	payload, err := json.MarshalIndent(normalized, "", "  ")
	if err != nil {
		return fmt.Errorf("encode model costs file: %w", err)
	}
	payload = append(payload, '\n')

	if err := writeAtomic(path, payload); err != nil {
		return fmt.Errorf("write model costs file: %w", err)
	}
	return nil
}

func validateCost(cost ModelCost) error {
	if strings.TrimSpace(cost.Model) == "" {
		return errors.New("model must not be empty")
	}
	if cost.InputPer1KTokens < 0 || cost.OutputPer1KTokens < 0 || cost.CachedInputPer1KToken < 0 {
		return errors.New("token costs must be non-negative")
	}
	return nil
}

func normalizeCost(cost ModelCost) ModelCost {
	cost.Model = strings.ToLower(strings.TrimSpace(cost.Model))
	return cost
}

func cloneCosts(costs []ModelCost) []ModelCost {
	cloned := make([]ModelCost, len(costs))
	copy(cloned, costs)
	return cloned
}

func writeAtomic(path string, payload []byte) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("file path is empty")
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create parent dir: %w", err)
	}

	tmp := filepath.Join(dir, fmt.Sprintf(".%s.%s.tmp", filepath.Base(path), uuid.NewString()))
	if err := os.WriteFile(tmp, payload, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
