package nosql

import (
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Bucket name constants.
const (
	BucketClients         = "clients"
	BucketModelCosts      = "model_costs"
	BucketUsageEvents     = "usage_events"
	BucketAdminUsers      = "admin_users"
	BucketAdminAudit      = "admin_audit"
	BucketMeta            = "meta"
	BucketCopilotPools    = "copilot_pools"
	BucketCopilotAccounts = "copilot_accounts"
)

var allBuckets = []string{
	BucketClients, BucketModelCosts, BucketUsageEvents,
	BucketAdminUsers, BucketAdminAudit, BucketMeta,
	BucketCopilotPools, BucketCopilotAccounts,
}

// OpenDB opens a bbolt database and creates all required buckets.
func OpenDB(path string) (*bolt.DB, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open bolt db: %w", err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, name := range allBuckets {
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return fmt.Errorf("create bucket %q: %w", name, err)
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}
