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
	EndpointType          string  `json:"endpoint_type,omitempty"`
	Model                 string  `json:"model"`
	InputPer1MTokens      float64 `json:"input_per_1m_tokens"`
	OutputPer1MTokens     float64 `json:"output_per_1m_tokens"`
	CachedInputPer1MToken float64 `json:"cached_input_per_1m_tokens"`
}

// ModelCostStore manages model costs backed by bbolt.
type ModelCostStore struct {
	db *bolt.DB
}

// NewModelCostStore creates a new bbolt-backed model cost store.
func NewModelCostStore(db *bolt.DB) *ModelCostStore {
	return &ModelCostStore{db: db}
}

// costKey builds the composite key "endpoint_type:model".
func costKey(cost ModelCost) string {
	c := normalizeCost(cost)
	return c.EndpointType + ":" + c.Model
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

// Delete removes all model costs matching the given model name (any endpoint_type).
// Deprecated: use DeleteByKey for endpoint_type-aware deletion.
func (s *ModelCostStore) Delete(model string) error {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return errors.New("model must not be empty")
	}
	suffix := ":" + model

	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketModelCosts))
		removed := false

		// Collect keys to delete (cannot delete during ForEach).
		var toDelete [][]byte
		err := b.ForEach(func(k, v []byte) error {
			if strings.HasSuffix(string(k), suffix) {
				toDelete = append(toDelete, append([]byte(nil), k...))
			}
			return nil
		})
		if err != nil {
			return err
		}

		for _, k := range toDelete {
			if err := b.Delete(k); err != nil {
				return err
			}
			removed = true
		}
		if !removed {
			return fmt.Errorf("model %q not found", model)
		}
		return nil
	})
}

// DeleteByKey removes one model cost by endpoint_type + model.
func (s *ModelCostStore) DeleteByKey(endpointType, model string) error {
	model = strings.ToLower(strings.TrimSpace(model))
	endpointType = strings.ToLower(strings.TrimSpace(endpointType))
	if endpointType == "" {
		endpointType = "azure_openai"
	}
	if model == "" {
		return errors.New("model must not be empty")
	}
	key := endpointType + ":" + model

	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketModelCosts))
		if b.Get([]byte(key)) == nil {
			return fmt.Errorf("model %q (endpoint_type=%q) not found", model, endpointType)
		}
		return b.Delete([]byte(key))
	})
}

func validateCost(cost ModelCost) error {
	if strings.TrimSpace(cost.Model) == "" {
		return errors.New("model must not be empty")
	}
	if cost.InputPer1MTokens < 0 || cost.OutputPer1MTokens < 0 || cost.CachedInputPer1MToken < 0 {
		return errors.New("token costs must be non-negative")
	}
	return nil
}

func normalizeCost(cost ModelCost) ModelCost {
	cost.Model = strings.ToLower(strings.TrimSpace(cost.Model))
	cost.EndpointType = strings.ToLower(strings.TrimSpace(cost.EndpointType))
	if cost.EndpointType == "" {
		cost.EndpointType = "azure_openai"
	}
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
