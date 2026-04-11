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
	DefaultQuotaSyncInterval = 5 * time.Minute

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
	PercentRemaining float64 `json:"percent_remaining"` // 0-100
	ResetAt          string  `json:"reset_at"`          // RFC3339
	CopilotPlan      string  `json:"copilot_plan"`
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
type gitHubCopilotUserResponse struct {
	CopilotPlan    string `json:"copilotPlan"`
	QuotaSnapshots struct {
		PremiumInteractions struct {
			PercentRemaining float64 `json:"percentRemaining"`
		} `json:"premiumInteractions"`
	} `json:"quotaSnapshots"`
}

// SyncQuotaFromGitHub 从 GitHub API 同步单个账户的额度。
// GET quotaURL
// Authorization: token <oauthToken>
// 加上 Editor Headers（直接设置，不依赖 headers.go）
func (m *QuotaManager) SyncQuotaFromGitHub(ctx context.Context, oauthToken string) (*QuotaInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.quotaURL, nil)
	if err != nil {
		return nil, fmt.Errorf("创建额度查询请求: %w", err)
	}

	req.Header.Set("Authorization", "token "+oauthToken)
	req.Header.Set("Accept", "application/json")
	// Editor Headers — 直接使用字面量，不依赖 headers.go
	req.Header.Set("Editor-Version", "vscode/1.96.2")
	req.Header.Set("Editor-Plugin-Version", "copilot/1.254.0")
	req.Header.Set("User-Agent", "GitHubCopilotChat/0.24.2024")
	req.Header.Set("Copilot-Integration-Id", "vscode-chat")

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

	return &QuotaInfo{
		PercentRemaining: result.QuotaSnapshots.PremiumInteractions.PercentRemaining,
		CopilotPlan:      result.CopilotPlan,
	}, nil
}

// DeductQuota 根据模型乘数扣减账户额度（本地计算）。
// 扣减量 = (multiplier / DefaultMonthlyPremiumRequests) * 100（百分比）
// 如果模型免费（乘数=0），不扣减。
// 扣减后 percentRemaining < 0 则设为 0。
func DeductQuota(account *nosql.CopilotAccount, model string) {
	multiplier := GetMultiplier(model)
	if multiplier == 0 {
		return
	}

	deduction := (multiplier / DefaultMonthlyPremiumRequests) * 100
	account.QuotaPercentRemaining -= deduction
	if account.QuotaPercentRemaining < 0 {
		account.QuotaPercentRemaining = 0
	}
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
