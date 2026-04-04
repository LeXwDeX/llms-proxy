package nosql

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	bolt "go.etcd.io/bbolt"

	"github.com/ycgame/llms-proxy/internal/config"
)

// ClientStore manages clients backed by bbolt.
type ClientStore struct {
	db *bolt.DB
}

// NewClientStore creates a new bbolt-backed client store.
func NewClientStore(db *bolt.DB) *ClientStore {
	return &ClientStore{db: db}
}

// List returns all clients from the store.
func (s *ClientStore) List() ([]config.Client, error) {
	var clients []config.Client
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketClients))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			var c config.Client
			if err := json.Unmarshal(v, &c); err != nil {
				return fmt.Errorf("decode client %q: %w", string(k), err)
			}
			clients = append(clients, c)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return cloneClients(clients), nil
}

// Create appends a new client. It fails when name or access_key already exists.
func (s *ClientStore) Create(client config.Client) error {
	if err := validateClient(client); err != nil {
		return err
	}
	client = normalizeClient(client)
	key := strings.ToLower(client.Name)

	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketClients))

		// Check name uniqueness.
		if b.Get([]byte(key)) != nil {
			return fmt.Errorf("client %q already exists", client.Name)
		}

		// Check access_key uniqueness by scanning the bucket.
		if err := checkAccessKeyUnique(b, "", client.AccessKey); err != nil {
			return err
		}

		data, err := json.Marshal(client)
		if err != nil {
			return fmt.Errorf("encode client: %w", err)
		}
		return b.Put([]byte(key), data)
	})
}

// Update updates one client by name.
func (s *ClientStore) Update(name string, client config.Client) error {
	if err := validateClient(client); err != nil {
		return err
	}

	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("name must not be empty")
	}
	oldKey := strings.ToLower(name)

	next := normalizeClient(client)
	newKey := strings.ToLower(next.Name)

	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketClients))

		// Check old record exists.
		if b.Get([]byte(oldKey)) == nil {
			return fmt.Errorf("client %q not found", name)
		}

		// If renaming, check new name doesn't conflict.
		if newKey != oldKey {
			if b.Get([]byte(newKey)) != nil {
				return fmt.Errorf("client %q already exists", next.Name)
			}
		}

		// Check access_key uniqueness (exclude the old record).
		if err := checkAccessKeyUnique(b, oldKey, next.AccessKey); err != nil {
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
			return fmt.Errorf("encode client: %w", err)
		}
		return b.Put([]byte(newKey), data)
	})
}

// Delete removes one client by name.
func (s *ClientStore) Delete(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("name must not be empty")
	}
	key := strings.ToLower(name)

	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketClients))
		if b.Get([]byte(key)) == nil {
			return fmt.Errorf("client %q not found", name)
		}
		return b.Delete([]byte(key))
	})
}

// checkAccessKeyUnique scans the bucket to ensure no other client (excluding
// excludeKey) uses the given access_key.
func checkAccessKeyUnique(b *bolt.Bucket, excludeKey, accessKey string) error {
	accessKey = strings.TrimSpace(accessKey)
	return b.ForEach(func(k, v []byte) error {
		if string(k) == excludeKey {
			return nil
		}
		var c config.Client
		if err := json.Unmarshal(v, &c); err != nil {
			return nil // skip corrupt entries
		}
		if strings.TrimSpace(c.AccessKey) == accessKey {
			return errors.New("access_key already exists")
		}
		return nil
	})
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
	if clients == nil {
		return nil
	}
	cloned := make([]config.Client, len(clients))
	for i := range clients {
		cloned[i] = clients[i]
		if len(clients[i].AllowedTargets) > 0 {
			cloned[i].AllowedTargets = append([]string(nil), clients[i].AllowedTargets...)
		}
	}
	return cloned
}
