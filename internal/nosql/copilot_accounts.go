package nosql

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

// 账户状态常量
const (
	AccountStatusPendingAuth   = "pending_auth"
	AccountStatusActive        = "active"
	AccountStatusTokenExpired  = "token_expired"
	AccountStatusQuotaExceeded = "quota_exceeded"
	AccountStatusDisabled      = "disabled"
)

// CopilotAccount 表示池内的单个 Copilot 订阅账户。
type CopilotAccount struct {
	ID             string `json:"id"`              // UUID 主键
	PoolName       string `json:"pool_name"`       // FK → CopilotPool.Name
	GitHubUsername string `json:"github_username"` // 授权后填入
	SortOrder      int    `json:"sort_order"`      // 1-based 顺序号
	Status         string `json:"status"`          // pending_auth|active|token_expired|quota_exceeded|disabled

	// OAuth tokens（明文存储，与项目现有 APIKey 安全模式一致）
	OAuthToken            string `json:"oauth_token"`              // gho_* 持久化 token
	CopilotToken          string `json:"copilot_token"`            // 短期 copilot access token
	CopilotTokenExpiresAt int64  `json:"copilot_token_expires_at"` // unix timestamp
	APIBaseURL            string `json:"api_base_url,omitempty"`   // 动态 API 端点（business/individual）

	// Device Flow 临时字段（仅 pending_auth 状态有值）
	DeviceCode          string `json:"device_code,omitempty"`
	UserCode            string `json:"user_code,omitempty"`
	VerificationURI     string `json:"verification_uri,omitempty"`
	DeviceCodeExpiresAt int64  `json:"device_code_expires_at,omitempty"`
	PollInterval        int    `json:"poll_interval,omitempty"`

	// 额度
	QuotaPercentRemaining float64 `json:"quota_percent_remaining"`     // 0-100
	QuotaEntitlement      int     `json:"quota_entitlement,omitempty"` // 月度总 premium requests（从 GitHub API 获取）
	QuotaRemaining        int     `json:"quota_remaining,omitempty"`   // 剩余 premium requests（从 GitHub API 获取）
	QuotaResetAt          string  `json:"quota_reset_at,omitempty"`
	QuotaLastSyncAt       string  `json:"quota_last_sync_at,omitempty"`
	QuotaBillingModel     string  `json:"quota_billing_model,omitempty"` // "credits" 或 "pru"
	QuotaUnlimited        bool    `json:"quota_unlimited,omitempty"`     // Business/Enterprise 无限额度
	AllowOverage          bool    `json:"allow_overage"`                 // 允许超额调用（额度耗尽后仍可使用付费模型）

	// 元数据
	Notes     string `json:"notes,omitempty"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// CopilotAccountStore 管理基于 bbolt 的 Copilot 账户存储。
type CopilotAccountStore struct {
	db        *bolt.DB
	poolStore *CopilotPoolStore
}

// NewCopilotAccountStore 创建一个新的 bbolt 支持的 Copilot 账户存储。
func NewCopilotAccountStore(db *bolt.DB, poolStore *CopilotPoolStore) *CopilotAccountStore {
	return &CopilotAccountStore{db: db, poolStore: poolStore}
}

// Create 创建一个新的 Copilot 账户。
// 验证：PoolName 非空、Pool 存在、账户数不超过 MaxAccounts。
// 自动：生成 ID、设置 CreatedAt/UpdatedAt、自动 SortOrder、默认 Status。
func (s *CopilotAccountStore) Create(account CopilotAccount) error {
	account.PoolName = strings.TrimSpace(account.PoolName)
	if account.PoolName == "" {
		return errors.New("pool_name must not be empty")
	}

	// 验证 Pool 存在
	pool, err := s.poolStore.Get(account.PoolName)
	if err != nil {
		return fmt.Errorf("pool %q not found", account.PoolName)
	}

	// 如果 Status 为空，默认设为 pending_auth
	account.Status = strings.TrimSpace(account.Status)
	if account.Status == "" {
		account.Status = AccountStatusPendingAuth
	}
	if !isValidAccountStatus(account.Status) {
		return fmt.Errorf("invalid status %q", account.Status)
	}

	// 自动生成 ID
	account.ID = generateUUID()

	now := time.Now().UTC().Format(time.RFC3339)
	account.CreatedAt = now
	account.UpdatedAt = now

	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketCopilotAccounts))

		// 统计同一 Pool 下的现有账户数和最大 SortOrder
		var count int
		var maxSortOrder int
		poolNameLower := strings.ToLower(account.PoolName)
		if err := b.ForEach(func(k, v []byte) error {
			var a CopilotAccount
			if err := json.Unmarshal(v, &a); err != nil {
				return nil // 跳过损坏记录
			}
			if strings.ToLower(strings.TrimSpace(a.PoolName)) == poolNameLower {
				count++
				if a.SortOrder > maxSortOrder {
					maxSortOrder = a.SortOrder
				}
			}
			return nil
		}); err != nil {
			return err
		}

		// 验证账户数不超过 MaxAccounts
		maxAccounts := pool.GetMaxAccounts()
		if count >= maxAccounts {
			return fmt.Errorf("pool %q already has %d accounts (max %d)", account.PoolName, count, maxAccounts)
		}

		// 自动设置 SortOrder
		if account.SortOrder == 0 {
			account.SortOrder = maxSortOrder + 1
		}

		data, err := json.Marshal(account)
		if err != nil {
			return fmt.Errorf("encode copilot_account: %w", err)
		}
		return b.Put([]byte(account.ID), data)
	})
}

// Get 按 ID 查询单个 Copilot 账户。
func (s *CopilotAccountStore) Get(id string) (*CopilotAccount, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("id must not be empty")
	}

	var account *CopilotAccount
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketCopilotAccounts))
		if b == nil {
			return fmt.Errorf("copilot_account %q not found", id)
		}
		v := b.Get([]byte(id))
		if v == nil {
			return fmt.Errorf("copilot_account %q not found", id)
		}
		var a CopilotAccount
		if err := json.Unmarshal(v, &a); err != nil {
			return fmt.Errorf("decode copilot_account %q: %w", id, err)
		}
		account = &a
		return nil
	})
	if err != nil {
		return nil, err
	}
	return account, nil
}

// Update 按 ID 更新 Copilot 账户（ID 不可变）。
func (s *CopilotAccountStore) Update(id string, account CopilotAccount) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("id must not be empty")
	}

	// 强制 ID 不可变
	account.ID = id
	account.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketCopilotAccounts))

		// 检查旧记录存在
		if b.Get([]byte(id)) == nil {
			return fmt.Errorf("copilot_account %q not found", id)
		}

		data, err := json.Marshal(account)
		if err != nil {
			return fmt.Errorf("encode copilot_account: %w", err)
		}
		return b.Put([]byte(id), data)
	})
}

// Delete 按 ID 删除 Copilot 账户。
func (s *CopilotAccountStore) Delete(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("id must not be empty")
	}

	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketCopilotAccounts))
		if b.Get([]byte(id)) == nil {
			return fmt.Errorf("copilot_account %q not found", id)
		}
		return b.Delete([]byte(id))
	})
}

// List 列出所有 Copilot 账户。
func (s *CopilotAccountStore) List() ([]CopilotAccount, error) {
	var accounts []CopilotAccount
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketCopilotAccounts))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			var a CopilotAccount
			if err := json.Unmarshal(v, &a); err != nil {
				return fmt.Errorf("decode copilot_account %q: %w", string(k), err)
			}
			accounts = append(accounts, a)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return accounts, nil
}

// ListByPool 按 PoolName 过滤 Copilot 账户，结果按 SortOrder 升序排序。
func (s *CopilotAccountStore) ListByPool(poolName string) ([]CopilotAccount, error) {
	poolName = strings.TrimSpace(poolName)
	if poolName == "" {
		return nil, errors.New("pool_name must not be empty")
	}

	poolNameLower := strings.ToLower(poolName)
	var accounts []CopilotAccount
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketCopilotAccounts))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			var a CopilotAccount
			if err := json.Unmarshal(v, &a); err != nil {
				return fmt.Errorf("decode copilot_account %q: %w", string(k), err)
			}
			if strings.ToLower(strings.TrimSpace(a.PoolName)) == poolNameLower {
				accounts = append(accounts, a)
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	// 按 SortOrder 升序排序
	sort.Slice(accounts, func(i, j int) bool {
		return accounts[i].SortOrder < accounts[j].SortOrder
	})

	return accounts, nil
}

// isValidAccountStatus 验证账户状态值是否合法。
func isValidAccountStatus(status string) bool {
	switch status {
	case AccountStatusPendingAuth,
		AccountStatusActive,
		AccountStatusTokenExpired,
		AccountStatusQuotaExceeded,
		AccountStatusDisabled:
		return true
	default:
		return false
	}
}

// generateUUID 生成 UUID v4（不引入第三方库）。
func generateUUID() string {
	var uuid [16]byte
	_, _ = rand.Read(uuid[:])
	// 设置版本号 (v4) 和变体位
	uuid[6] = (uuid[6] & 0x0f) | 0x40 // version 4
	uuid[8] = (uuid[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		uuid[0:4], uuid[4:6], uuid[6:8], uuid[8:10], uuid[10:16])
}
