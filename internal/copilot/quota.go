package copilot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/ycgame/llms-proxy/internal/nosql"
)

// 额度相关常量
const (
	// 默认同步间隔
	DefaultQuotaSyncInterval = 2 * time.Minute

	// 额度耗尽阈值
	QuotaExhaustedThreshold = 0.0

	// Premium request 总量（用于百分比计算）
	// GitHub Copilot Pro 月度额度约 300 premium requests
	DefaultMonthlyPremiumRequests = 300.0

	// 默认额度查询 API URL
	defaultQuotaURL = "https://api.github.com/copilot_internal/user"
)

// QuotaInfo 表示从 GitHub API 获取的额度信息
type QuotaInfo struct {
	PercentRemaining float64 `json:"percent_remaining"` // 可为负数（超额使用）
	ResetAt          string  `json:"reset_at"`          // RFC3339 或 YYYY-MM-DD
	CopilotPlan      string  `json:"copilot_plan"`
	Unlimited        bool    `json:"unlimited"`    // business 的 chat/completions 为 true
	Entitlement      int     `json:"entitlement"`  // 月度总额度（premium requests）
	Remaining        int     `json:"remaining"`    // 剩余 premium requests
	BillingModel     string  `json:"billing_model"` // "credits" 或 "pru"
}

// QuotaManager 管理 Copilot 账户额度
type QuotaManager struct {
	httpClient *http.Client
	quotaURL   string // 可测试替换，默认 https://api.github.com/copilot_internal/user
	logger     *slog.Logger
	mu         sync.Mutex
	stopCh     chan struct{}
	stopped    bool
}

// NewQuotaManager 创建额度管理器。
func NewQuotaManager(httpClient *http.Client, quotaURL string, logger *slog.Logger) *QuotaManager {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if quotaURL == "" {
		quotaURL = defaultQuotaURL
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &QuotaManager{
		httpClient: httpClient,
		quotaURL:   quotaURL,
		logger:     logger,
		stopCh:     make(chan struct{}),
	}
}

// gitHubCopilotUserResponse 表示 GitHub Copilot user API 的响应结构。
// API 同时返回驼峰和蛇形字段名，此处两种都标注以保证兼容。
type gitHubCopilotUserResponse struct {
	// copilot_plan / copilotPlan
	CopilotPlan string `json:"copilot_plan"`

	// quota_reset_date_utc（RFC3339）
	QuotaResetDateUTC string `json:"quota_reset_date_utc"`
	// quota_reset_date（YYYY-MM-DD）
	QuotaResetDate string `json:"quota_reset_date"`

	// 新版 snake_case 格式（实际 API 使用）
	QuotaSnapshots *quotaSnapshotsSnake `json:"quota_snapshots,omitempty"`

	// 旧版 camelCase 格式（兼容）
	QuotaSnapshotsCamel *quotaSnapshotsCamel `json:"quotaSnapshots,omitempty"`
}

// snake_case 格式的快照
type quotaSnapshotsSnake struct {
	PremiumInteractions *quotaEntrySnake `json:"premium_interactions,omitempty"`
	AICredits           *quotaEntrySnake `json:"ai_credits,omitempty"`
	Chat                *quotaEntrySnake `json:"chat,omitempty"`
}

type quotaEntrySnake struct {
	PercentRemaining float64 `json:"percent_remaining"`
	QuotaRemaining   float64 `json:"quota_remaining"`
	Unlimited        bool    `json:"unlimited"`
	Entitlement      int     `json:"entitlement"`
	Remaining        int     `json:"remaining"`
	OverageCount     int     `json:"overage_count"`
	OveragePermitted bool    `json:"overage_permitted"`
}

// camelCase 格式的快照（向后兼容）
type quotaSnapshotsCamel struct {
	PremiumInteractions *quotaEntryCamel `json:"premiumInteractions,omitempty"`
}

type quotaEntryCamel struct {
	PercentRemaining float64 `json:"percentRemaining"`
}

// extractQuotaInfo 从 API 响应中提取额度信息，自动处理新旧格式。
func (r *gitHubCopilotUserResponse) extractQuotaInfo() *QuotaInfo {
	info := &QuotaInfo{
		CopilotPlan: r.CopilotPlan,
	}

	// 优先使用 quota_reset_date_utc（精确到时间），其次 quota_reset_date
	if r.QuotaResetDateUTC != "" {
		info.ResetAt = r.QuotaResetDateUTC
	} else if r.QuotaResetDate != "" {
		info.ResetAt = r.QuotaResetDate
	}

	// 优先使用新版 snake_case 格式
	if r.QuotaSnapshots != nil {
		// 优先尝试 AI Credits schema（2026-06-01 后）
		if r.QuotaSnapshots.AICredits != nil {
			ai := r.QuotaSnapshots.AICredits
			info.PercentRemaining = ai.PercentRemaining
			info.Unlimited = ai.Unlimited
			info.Entitlement = ai.Entitlement
			info.Remaining = ai.Remaining
			info.BillingModel = "credits"
			return info
		}
		// 回退到 legacy PRU schema（年付用户）
		if r.QuotaSnapshots.PremiumInteractions != nil {
			pi := r.QuotaSnapshots.PremiumInteractions
			info.PercentRemaining = pi.PercentRemaining
			info.Unlimited = pi.Unlimited
			info.Entitlement = pi.Entitlement
			info.Remaining = pi.Remaining
			info.BillingModel = "pru"
			return info
		}
	}

	// 兜底：旧版 camelCase 格式
	if r.QuotaSnapshotsCamel != nil && r.QuotaSnapshotsCamel.PremiumInteractions != nil {
		info.PercentRemaining = r.QuotaSnapshotsCamel.PremiumInteractions.PercentRemaining
		return info
	}

	return info
}

// SyncQuotaFromGitHub 从 GitHub API 同步单个账户的额度。
// GET quotaURL
// Authorization: token <oauthToken>
func (m *QuotaManager) SyncQuotaFromGitHub(ctx context.Context, oauthToken string) (*QuotaInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.quotaURL, nil)
	if err != nil {
		return nil, fmt.Errorf("创建额度查询请求: %w", err)
	}

	req.Header.Set("Authorization", "token "+oauthToken)
	req.Header.Set("Accept", "application/json")
	ApplyEditorHeaders(req)

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("发送额度查询请求: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取额度查询响应: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("额度查询请求失败: status=%d, body=%s", resp.StatusCode, string(body))
	}

	var result gitHubCopilotUserResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("解析额度查询响应: %w", err)
	}

	return result.extractQuotaInfo(), nil
}

// DeductQuota 根据模型乘数扣减账户额度（本地计算）。
// 扣减量 = (multiplier / DefaultMonthlyPremiumRequests) * 100（百分比）
// 如果模型免费（乘数=0），不扣减。
// 允许 percentRemaining 为负数以反映超额使用。
func DeductQuota(account *nosql.CopilotAccount, model string) {
	multiplier := GetMultiplier(model)
	if multiplier == 0 {
		return
	}

	deduction := (multiplier / DefaultMonthlyPremiumRequests) * 100
	account.QuotaPercentRemaining -= deduction
}

// IsQuotaExhausted 检查账户额度是否耗尽。
func IsQuotaExhausted(account *nosql.CopilotAccount) bool {
	return account.QuotaPercentRemaining <= QuotaExhaustedThreshold
}

// StartPeriodicSync 启动后台 goroutine 定期同步所有 active 账户额度。
// 每次同步：
// 1. 列出所有账户
// 2. 过滤 active 和 quota_exceeded 状态
// 3. 对每个账户调用 SyncQuotaFromGitHub
// 4. 更新 store 中的 QuotaPercentRemaining、QuotaResetAt、QuotaLastSyncAt
// 5. 如果 percentRemaining <= 0，设置状态为 quota_exceeded
// 6. 如果之前是 quota_exceeded 但 percentRemaining > 0（月度重置），恢复为 active
func (m *QuotaManager) StartPeriodicSync(ctx context.Context, store *nosql.CopilotAccountStore, interval time.Duration) {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()

	go func() {
		// 首次立即同步一次
		m.syncAllAccounts(ctx, store)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-m.stopCh:
				return
			case <-ticker.C:
				m.syncAllAccounts(ctx, store)
			}
		}
	}()
}

// Stop 停止后台同步。
func (m *QuotaManager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.stopped {
		m.stopped = true
		close(m.stopCh)
	}
}

// syncAllAccounts 同步所有 active/quota_exceeded 账户的额度。
func (m *QuotaManager) syncAllAccounts(ctx context.Context, store *nosql.CopilotAccountStore) {
	accounts, err := store.List()
	if err != nil {
		m.logger.Error("列出 copilot 账户失败", "error", err)
		return
	}

	for i := range accounts {
		account := &accounts[i]

		// 只处理 active 和 quota_exceeded 状态的账户
		if account.Status != nosql.AccountStatusActive && account.Status != nosql.AccountStatusQuotaExceeded {
			continue
		}

		// 需要有 OAuth token 才能查询
		if account.OAuthToken == "" {
			continue
		}

		quotaInfo, err := m.SyncQuotaFromGitHub(ctx, account.OAuthToken)
		if err != nil {
			m.logger.Warn("同步账户额度失败",
				"account_id", account.ID,
				"error", err,
			)
			continue
		}

		// 更新额度信息
		account.QuotaPercentRemaining = quotaInfo.PercentRemaining
		if quotaInfo.ResetAt != "" {
			account.QuotaResetAt = quotaInfo.ResetAt
		}
		account.QuotaLastSyncAt = time.Now().UTC().Format(time.RFC3339)
		if quotaInfo.Entitlement > 0 {
			account.QuotaEntitlement = quotaInfo.Entitlement
		}
		account.QuotaRemaining = quotaInfo.Remaining
		account.QuotaBillingModel = quotaInfo.BillingModel

		// 根据额度调整状态
		if quotaInfo.PercentRemaining <= QuotaExhaustedThreshold {
			account.Status = nosql.AccountStatusQuotaExceeded
		} else if account.Status == nosql.AccountStatusQuotaExceeded {
			// 之前是 quota_exceeded 但额度恢复了（月度重置），恢复为 active
			account.Status = nosql.AccountStatusActive
		}

		if err := store.Update(account.ID, *account); err != nil {
			m.logger.Error("更新账户额度失败",
				"account_id", account.ID,
				"error", err,
			)
		}
	}
}
