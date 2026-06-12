package nosql

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	bolt "go.etcd.io/bbolt"
)

// ModelCost defines token pricing (per 1M tokens).
type ModelCost struct {
	EndpointType          string  `json:"endpoint_type,omitempty"` // deprecated, not used as key
	Model                 string  `json:"model"`
	InputPer1MTokens      float64 `json:"input_per_1m_tokens"`
	OutputPer1MTokens     float64 `json:"output_per_1m_tokens"`
	CachedInputPer1MToken float64 `json:"cached_input_per_1m_tokens"`
	CacheReadPer1MToken   float64 `json:"cache_read_per_1m_tokens"`
}

// ModelCostStore manages model costs backed by bbolt.
type ModelCostStore struct {
	db *bolt.DB
}

// NewModelCostStore creates a new bbolt-backed model cost store.
func NewModelCostStore(db *bolt.DB) *ModelCostStore {
	return &ModelCostStore{db: db}
}

// costKey builds the key from model name only.
func costKey(cost ModelCost) string {
	return strings.ToLower(strings.TrimSpace(cost.Model))
}

// List returns all model costs.
func (s *ModelCostStore) List() ([]ModelCost, error) {
	var costs []ModelCost
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketModelCosts))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			var c ModelCost
			if err := json.Unmarshal(v, &c); err != nil {
				return fmt.Errorf("decode model cost %q: %w", string(k), err)
			}
			costs = append(costs, c)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return cloneCosts(costs), nil
}

// Upsert inserts or updates one model cost.
func (s *ModelCostStore) Upsert(cost ModelCost) error {
	if err := validateCost(cost); err != nil {
		return err
	}
	cost = normalizeCost(cost)
	key := costKey(cost)

	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketModelCosts))
		data, err := json.Marshal(cost)
		if err != nil {
			return fmt.Errorf("encode model cost: %w", err)
		}
		return b.Put([]byte(key), data)
	})
}

// Delete removes one model cost by model name.
func (s *ModelCostStore) Delete(model string) error {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return errors.New("model must not be empty")
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketModelCosts))
		if b.Get([]byte(model)) == nil {
			return fmt.Errorf("model %q not found", model)
		}
		return b.Delete([]byte(model))
	})
}

// DeleteByKey is deprecated; kept for backward compatibility. Delegates to Delete.
func (s *ModelCostStore) DeleteByKey(endpointType, model string) error {
	return s.Delete(model)
}

// MigrateKeys migrates old "endpoint_type:model" keys to model-only keys.
// Safe to call multiple times; idempotent.
func (s *ModelCostStore) MigrateKeys() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketModelCosts))
		if b == nil {
			return nil
		}
		var toMigrate []struct {
			oldKey []byte
			cost   ModelCost
		}
		err := b.ForEach(func(k, v []byte) error {
			key := string(k)
			if !strings.Contains(key, ":") {
				return nil // already model-only key
			}
			var c ModelCost
			if err := json.Unmarshal(v, &c); err != nil {
				return nil // skip corrupt entries
			}
			toMigrate = append(toMigrate, struct {
				oldKey []byte
				cost   ModelCost
			}{append([]byte(nil), k...), c})
			return nil
		})
		if err != nil {
			return err
		}
		for _, m := range toMigrate {
			newKey := strings.ToLower(strings.TrimSpace(m.cost.Model))
			if newKey == "" {
				continue
			}
			// Write under new key (last-writer-wins if conflict)
			data, err := json.Marshal(m.cost)
			if err != nil {
				continue
			}
			_ = b.Put([]byte(newKey), data)
			_ = b.Delete(m.oldKey)
		}
		return nil
	})
}

func validateCost(cost ModelCost) error {
	if strings.TrimSpace(cost.Model) == "" {
		return errors.New("model must not be empty")
	}
	if cost.InputPer1MTokens < 0 || cost.OutputPer1MTokens < 0 || cost.CachedInputPer1MToken < 0 || cost.CacheReadPer1MToken < 0 {
		return errors.New("token costs must be non-negative")
	}
	return nil
}

func normalizeCost(cost ModelCost) ModelCost {
	cost.Model = strings.ToLower(strings.TrimSpace(cost.Model))
	cost.EndpointType = strings.ToLower(strings.TrimSpace(cost.EndpointType))
	return cost
}

func cloneCosts(costs []ModelCost) []ModelCost {
	if costs == nil {
		return nil
	}
	cloned := make([]ModelCost, len(costs))
	copy(cloned, costs)
	return cloned
}
