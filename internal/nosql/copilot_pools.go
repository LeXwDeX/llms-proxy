package nosql

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

// CopilotPool represents a Copilot subscription pool bound to exactly one client.
type CopilotPool struct {
	Name        string   `json:"name"`                   // Pool name (primary key)
	ClientName  string   `json:"client_name"`            // Bound client name (globally unique 1:1)
	Targets     []string `json:"targets"`                // Associated target names
	MaxAccounts int      `json:"max_accounts,omitempty"` // 每池最大账户数，默认 5
	Notes       string   `json:"notes,omitempty"`        // Optional notes
	UpdatedAt   string   `json:"updated_at,omitempty"`   // Auto-set on Create/Update (UTC RFC3339)
}

// GetMaxAccounts 返回池的最大账户数，未设置或非法值时返回默认值 5。
func (p *CopilotPool) GetMaxAccounts() int {
	if p.MaxAccounts <= 0 {
		return 5 // 默认值
	}
	return p.MaxAccounts
}

// CopilotPoolStore manages copilot pools backed by bbolt.
type CopilotPoolStore struct {
	db *bolt.DB
}

// NewCopilotPoolStore creates a new bbolt-backed copilot pool store.
func NewCopilotPoolStore(db *bolt.DB) *CopilotPoolStore {
	return &CopilotPoolStore{db: db}
}

// List returns all copilot pools from the store.
func (s *CopilotPoolStore) List() ([]CopilotPool, error) {
	var pools []CopilotPool
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketCopilotPools))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			var p CopilotPool
			if err := json.Unmarshal(v, &p); err != nil {
				return fmt.Errorf("decode copilot_pool %q: %w", string(k), err)
			}
			pools = append(pools, p)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return clonePools(pools), nil
}

// Get retrieves a single copilot pool by name.
func (s *CopilotPoolStore) Get(name string) (*CopilotPool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("name must not be empty")
	}
	key := strings.ToLower(name)

	var pool *CopilotPool
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketCopilotPools))
		if b == nil {
			return fmt.Errorf("copilot_pool %q not found", name)
		}
		v := b.Get([]byte(key))
		if v == nil {
			return fmt.Errorf("copilot_pool %q not found", name)
		}
		var p CopilotPool
		if err := json.Unmarshal(v, &p); err != nil {
			return fmt.Errorf("decode copilot_pool %q: %w", key, err)
		}
		pool = &p
		return nil
	})
	if err != nil {
		return nil, err
	}
	return pool, nil
}

// Create stores a new copilot pool.
// Constraints: name unique, client_name globally unique (1:1).
func (s *CopilotPoolStore) Create(pool CopilotPool) error {
	if err := validatePool(pool); err != nil {
		return err
	}
	pool = normalizePool(pool)
	pool.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	key := strings.ToLower(pool.Name)

	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketCopilotPools))

		// Check name uniqueness.
		if b.Get([]byte(key)) != nil {
			return fmt.Errorf("copilot_pool %q already exists", pool.Name)
		}

		// Check client_name uniqueness by scanning the bucket.
		if err := checkClientNameUnique(b, "", pool.ClientName); err != nil {
			return err
		}

		data, err := json.Marshal(pool)
		if err != nil {
			return fmt.Errorf("encode copilot_pool: %w", err)
		}
		return b.Put([]byte(key), data)
	})
}

// Update updates an existing copilot pool by name.
// Constraints: old record must exist, name conflict check on rename,
// client_name globally unique (excluding self).
func (s *CopilotPoolStore) Update(name string, pool CopilotPool) error {
	if err := validatePool(pool); err != nil {
		return err
	}

	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("name must not be empty")
	}
	oldKey := strings.ToLower(name)

	next := normalizePool(pool)
	next.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	newKey := strings.ToLower(next.Name)

	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketCopilotPools))

		// Check old record exists.
		if b.Get([]byte(oldKey)) == nil {
			return fmt.Errorf("copilot_pool %q not found", name)
		}

		// If renaming, check new name doesn't conflict.
		if newKey != oldKey {
			if b.Get([]byte(newKey)) != nil {
				return fmt.Errorf("copilot_pool %q already exists", next.Name)
			}
		}

		// Check client_name uniqueness (exclude the old record).
		if err := checkClientNameUnique(b, oldKey, next.ClientName); err != nil {
			return err
		}

		// Delete old key if renamed.
		if newKey != oldKey {
			if err := b.Delete([]byte(oldKey)); err != nil {
				return err
			}
		}

		data, err := json.Marshal(next)
		if err != nil {
			return fmt.Errorf("encode copilot_pool: %w", err)
		}
		return b.Put([]byte(newKey), data)
	})
}

// Delete removes a copilot pool by name.
func (s *CopilotPoolStore) Delete(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("name must not be empty")
	}
	key := strings.ToLower(name)

	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketCopilotPools))
		if b.Get([]byte(key)) == nil {
			return fmt.Errorf("copilot_pool %q not found", name)
		}
		return b.Delete([]byte(key))
	})
}

// checkClientNameUnique scans the bucket to ensure no other pool (excluding
// excludeKey) uses the given client_name.
func checkClientNameUnique(b *bolt.Bucket, excludeKey, clientName string) error {
	clientName = strings.ToLower(strings.TrimSpace(clientName))
	return b.ForEach(func(k, v []byte) error {
		if string(k) == excludeKey {
			return nil
		}
		var p CopilotPool
		if err := json.Unmarshal(v, &p); err != nil {
			return nil // skip corrupt entries
		}
		if strings.ToLower(strings.TrimSpace(p.ClientName)) == clientName {
			return fmt.Errorf("client_name %q is already bound to pool %q", clientName, p.Name)
		}
		return nil
	})
}

func validatePool(pool CopilotPool) error {
	if strings.TrimSpace(pool.Name) == "" {
		return errors.New("name must not be empty")
	}
	if strings.TrimSpace(pool.ClientName) == "" {
		return errors.New("client_name must not be empty")
	}
	return nil
}

func normalizePool(pool CopilotPool) CopilotPool {
	pool.Name = strings.TrimSpace(pool.Name)
	pool.ClientName = strings.TrimSpace(pool.ClientName)
	pool.Notes = strings.TrimSpace(pool.Notes)

	normalizedTargets := make([]string, 0, len(pool.Targets))
	seen := make(map[string]struct{}, len(pool.Targets))
	for _, target := range pool.Targets {
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
	pool.Targets = normalizedTargets
	return pool
}

func clonePools(pools []CopilotPool) []CopilotPool {
	if pools == nil {
		return nil
	}
	cloned := make([]CopilotPool, len(pools))
	for i := range pools {
		cloned[i] = pools[i]
		if len(pools[i].Targets) > 0 {
			cloned[i].Targets = append([]string(nil), pools[i].Targets...)
		}
	}
	return cloned
}
