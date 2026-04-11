package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ycgame/llms-proxy/internal/nosql"
)

// CopilotService 聚合 OAuth、Token 管理和账户操作。
type CopilotService struct {
	accountStore *nosql.CopilotAccountStore
	poolStore    *nosql.CopilotPoolStore
	oauthClient  *OAuthClient
	tokenManager *TokenManager
	httpClient   *http.Client
	logger       *slog.Logger

	// 模型元数据内存缓存
	modelCache     []CopilotModelDetail
	modelCacheMu   sync.RWMutex
	modelCacheAt   time.Time
	modelRefreshMu sync.Mutex // 刷新互斥锁，防止 thundering herd
}

// NewCopilotService 创建服务。
func NewCopilotService(
	accountStore *nosql.CopilotAccountStore,
	poolStore *nosql.CopilotPoolStore,
	httpClient *http.Client,
	logger *slog.Logger,
) *CopilotService {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &CopilotService{
		accountStore: accountStore,
		poolStore:    poolStore,
		oauthClient:  NewOAuthClient(httpClient, "", ""),
		tokenManager: NewTokenManager(httpClient, ""),
		httpClient:   httpClient,
		logger:       logger,
	}
}

// InitiateAuth 发起 OAuth 授权。
// 1. 调用 oauthClient.StartDeviceFlow()
// 2. 创建 pending_auth 状态的 CopilotAccount，保存 DeviceCode、UserCode、VerificationURI
// 3. 返回 accountID、userCode、verificationURI
func (s *CopilotService) InitiateAuth(ctx context.Context, poolName string) (accountID, userCode, verificationURI string, err error) {
	s.logger.Info("发起 OAuth 授权", "pool", poolName)

	// 发起 Device Flow
	deviceResp, err := s.oauthClient.StartDeviceFlow(ctx)
	if err != nil {
		return "", "", "", fmt.Errorf("启动 device flow: %w", err)
	}

	// 创建 pending_auth 状态的账户
	account := nosql.CopilotAccount{
		PoolName:            poolName,
		Status:              nosql.AccountStatusPendingAuth,
		DeviceCode:          deviceResp.DeviceCode,
		UserCode:            deviceResp.UserCode,
		VerificationURI:     deviceResp.VerificationURI,
		DeviceCodeExpiresAt: time.Now().Unix() + int64(deviceResp.ExpiresIn),
		PollInterval:        deviceResp.Interval,
	}

	if err := s.accountStore.Create(account); err != nil {
		return "", "", "", fmt.Errorf("创建 pending_auth 账户: %w", err)
	}

	// 获取刚创建的账户 ID（通过 ListByPool 找到最新的 pending_auth 账户）
	accounts, err := s.accountStore.ListByPool(poolName)
	if err != nil {
		return "", "", "", fmt.Errorf("查询账户: %w", err)
	}

	// 找到刚创建的账户（匹配 DeviceCode）
	for _, a := range accounts {
		if a.DeviceCode == deviceResp.DeviceCode {
			s.logger.Info("OAuth 授权已发起",
				"account_id", a.ID,
				"user_code", deviceResp.UserCode,
				"verification_uri", deviceResp.VerificationURI,
			)
			return a.ID, deviceResp.UserCode, deviceResp.VerificationURI, nil
		}
	}

	return "", "", "", fmt.Errorf("未找到刚创建的账户")
}

// CompleteAuth 完成 OAuth 授权。
// 1. 读取账户，获取 DeviceCode 和 PollInterval
// 2. 调用 oauthClient.PollForToken() 获取 OAuth token
// 3. 获取 GitHub username
// 4. 获取初始 Copilot access token
// 5. 更新账户状态为 active，保存所有 token
// 6. 清除 Device Flow 临时字段
func (s *CopilotService) CompleteAuth(ctx context.Context, accountID string) error {
	s.logger.Info("完成 OAuth 授权", "account_id", accountID)

	account, err := s.accountStore.Get(accountID)
	if err != nil {
		return fmt.Errorf("获取账户 %q: %w", accountID, err)
	}

	if account.DeviceCode == "" {
		return fmt.Errorf("账户 %q 缺少 device code", accountID)
	}

	// 轮询获取 OAuth token
	oauthToken, err := s.oauthClient.PollForToken(ctx, account.DeviceCode, account.PollInterval)
	if err != nil {
		return fmt.Errorf("轮询 OAuth token: %w", err)
	}

	// 获取 GitHub username
	username, err := s.fetchGitHubUsername(ctx, oauthToken)
	if err != nil {
		s.logger.Warn("获取 GitHub username 失败，继续授权流程", "error", err)
		username = "" // 非致命错误
	}

	// 获取初始 Copilot access token
	tokenResp, err := s.tokenManager.FetchCopilotToken(ctx, oauthToken)
	if err != nil {
		return fmt.Errorf("获取 copilot token: %w", err)
	}

	// 更新账户
	account.Status = nosql.AccountStatusActive
	account.OAuthToken = oauthToken
	account.GitHubUsername = username
	account.CopilotToken = tokenResp.Token
	account.CopilotTokenExpiresAt = tokenResp.ExpiresAt
	account.APIBaseURL = tokenResp.APIBaseURL()

	// 清除 Device Flow 临时字段
	account.DeviceCode = ""
	account.UserCode = ""
	account.VerificationURI = ""
	account.DeviceCodeExpiresAt = 0
	account.PollInterval = 0

	if err := s.accountStore.Update(accountID, *account); err != nil {
		return fmt.Errorf("更新账户 %q: %w", accountID, err)
	}

	s.logger.Info("OAuth 授权完成",
		"account_id", accountID,
		"username", username,
		"status", nosql.AccountStatusActive,
	)

	return nil
}

// RevokeAuth 注销账户。
// 设置状态为 disabled，清除所有 token。
func (s *CopilotService) RevokeAuth(ctx context.Context, accountID string) error {
	s.logger.Info("注销账户", "account_id", accountID)

	account, err := s.accountStore.Get(accountID)
	if err != nil {
		return fmt.Errorf("获取账户 %q: %w", accountID, err)
	}

	account.Status = nosql.AccountStatusDisabled
	account.OAuthToken = ""
	account.CopilotToken = ""
	account.CopilotTokenExpiresAt = 0
	account.DeviceCode = ""
	account.UserCode = ""
	account.VerificationURI = ""
	account.DeviceCodeExpiresAt = 0
	account.PollInterval = 0

	if err := s.accountStore.Update(accountID, *account); err != nil {
		return fmt.Errorf("更新账户 %q: %w", accountID, err)
	}

	s.logger.Info("账户已注销", "account_id", accountID)
	return nil
}

// GetToken 获取有效的 Copilot access token（自动刷新）。
// 允许 active 和 quota_exceeded 状态（额度耗尽不影响 token 有效性）。
func (s *CopilotService) GetToken(ctx context.Context, accountID string) (string, error) {
	account, err := s.accountStore.Get(accountID)
	if err != nil {
		return "", fmt.Errorf("获取账户 %q: %w", accountID, err)
	}

	if account.Status != nosql.AccountStatusActive && account.Status != nosql.AccountStatusQuotaExceeded {
		return "", fmt.Errorf("账户 %q 状态为 %q，无法获取 token", accountID, account.Status)
	}

	return s.tokenManager.EnsureValidToken(ctx, account, s.accountStore)
}

// GetAccountStore 返回 account store（供 proxy 层使用）。
func (s *CopilotService) GetAccountStore() *nosql.CopilotAccountStore {
	return s.accountStore
}

// GetPoolStore 返回 pool store（供 proxy 层使用）。
func (s *CopilotService) GetPoolStore() *nosql.CopilotPoolStore {
	return s.poolStore
}

// MarkQuotaExhausted 将账户标记为额度耗尽状态。
func (s *CopilotService) MarkQuotaExhausted(accountID string, account *nosql.CopilotAccount) error {
	account.Status = nosql.AccountStatusQuotaExceeded
	return s.accountStore.Update(accountID, *account)
}

// FindPoolByClient 根据 client name 查找绑定的 CopilotPool。
func (s *CopilotService) FindPoolByClient(clientName string) (*nosql.CopilotPool, error) {
	pools, err := s.poolStore.List()
	if err != nil {
		return nil, fmt.Errorf("列出 pools: %w", err)
	}

	clientLower := strings.ToLower(strings.TrimSpace(clientName))
	for i := range pools {
		if strings.ToLower(strings.TrimSpace(pools[i].ClientName)) == clientLower {
			return &pools[i], nil
		}
	}

	return nil, fmt.Errorf("未找到 client %q 绑定的 copilot pool", clientName)
}

// SelectAccount 按 SortOrder 顺序选择最佳可用 Copilot 账户。
// 选号策略（按优先级）：
//  1. active 且有额度的账户（最优）
//  2. 已开启「允许超额」的 active 或 quota_exceeded 账户
//
// 免费模型不消耗额度，任何 active/quota_exceeded 账户均可。
func (s *CopilotService) SelectAccount(poolName, model string) (*nosql.CopilotAccount, error) {
	accounts, err := s.accountStore.ListByPool(poolName)
	if err != nil {
		return nil, fmt.Errorf("列出 pool %q 的账户: %w", poolName, err)
	}

	if len(accounts) == 0 {
		return nil, errors.New("pool 内无任何账户")
	}

	isFree := IsFreeModel(model)

	if isFree {
		for i := range accounts {
			a := &accounts[i]
			if a.Status == nosql.AccountStatusActive || a.Status == nosql.AccountStatusQuotaExceeded {
				return a, nil
			}
		}
		return nil, fmt.Errorf("pool %q 内无可用账户", poolName)
	}

	// 付费模型：两轮扫描
	for i := range accounts {
		a := &accounts[i]
		if a.Status == nosql.AccountStatusActive && !IsQuotaExhausted(a) {
			return a, nil
		}
	}
	for i := range accounts {
		a := &accounts[i]
		if a.AllowOverage && (a.Status == nosql.AccountStatusActive || a.Status == nosql.AccountStatusQuotaExceeded) {
			return a, nil
		}
	}

	return nil, fmt.Errorf("pool %q 内无可用账户（额度已耗尽且未开启超额调用）", poolName)
}

// DeductAndPersistQuota 扣减额度并持久化，如果额度耗尽则自动更新状态。
func (s *CopilotService) DeductAndPersistQuota(account *nosql.CopilotAccount, upstreamModel string) error {
	DeductQuota(account, upstreamModel)
	if err := s.accountStore.Update(account.ID, *account); err != nil {
		return fmt.Errorf("额度扣减写回失败: %w", err)
	}

	if IsQuotaExhausted(account) {
		account.Status = nosql.AccountStatusQuotaExceeded
		if err := s.accountStore.Update(account.ID, *account); err != nil {
			s.logger.Warn("额度耗尽状态更新失败", "account_id", account.ID, "error", err)
		}
	}
	return nil
}

// FetchAccountModels 为指定账户获取完整模型列表。
// 自动获取 token、确定 modelsURL。
func (s *CopilotService) FetchAccountModels(ctx context.Context, accountID string) ([]CopilotModelDetail, error) {
	token, err := s.GetToken(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("获取 token: %w", err)
	}
	account, err := s.accountStore.Get(accountID)
	if err != nil {
		return nil, fmt.Errorf("获取账户: %w", err)
	}
	modelsURL := CopilotModelsURL
	if account.APIBaseURL != "" {
		modelsURL = strings.TrimRight(account.APIBaseURL, "/") + "/models"
	}
	return FetchModelDetails(ctx, s.httpClient, token, modelsURL)
}

// fetchGitHubUsername 通过 GitHub API 获取用户名。
// GET https://api.github.com/user，Authorization: token <oauthToken>
func (s *CopilotService) fetchGitHubUsername(ctx context.Context, oauthToken string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/user", nil)
	if err != nil {
		return "", fmt.Errorf("创建 user 请求: %w", err)
	}
	req.Header.Set("Authorization", "token "+oauthToken)
	req.Header.Set("Accept", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("发送 user 请求: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("读取 user 响应: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("user 请求失败: status=%d, body=%s", resp.StatusCode, string(body))
	}

	var result struct {
		Login string `json:"login"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("解析 user 响应: %w", err)
	}

	return result.Login, nil
}

// modelCacheTTL 模型缓存有效期。
const modelCacheTTL = 10 * time.Minute

// GetCachedModels 返回缓存的完整模型元数据。
// 如果缓存不超过 10 分钟则直接返回，否则自动刷新。
func (s *CopilotService) GetCachedModels(ctx context.Context) ([]CopilotModelDetail, error) {
	s.modelCacheMu.RLock()
	if len(s.modelCache) > 0 && time.Since(s.modelCacheAt) < modelCacheTTL {
		cached := make([]CopilotModelDetail, len(s.modelCache))
		copy(cached, s.modelCache)
		s.modelCacheMu.RUnlock()
		return cached, nil
	}
	s.modelCacheMu.RUnlock()

	return s.refreshModelCacheInternal(ctx)
}

// RefreshModelCache 强制刷新模型缓存。
func (s *CopilotService) RefreshModelCache(ctx context.Context) error {
	_, err := s.refreshModelCacheInternal(ctx)
	return err
}

// refreshModelCacheInternal 使用第一个可用账户的 token 刷新模型缓存。
// 使用 modelRefreshMu 互斥锁 + 双检锁避免 thundering herd：
// 多个并发请求同时触发刷新时，只有第一个执行实际刷新，后续直接返回缓存。
func (s *CopilotService) refreshModelCacheInternal(ctx context.Context) ([]CopilotModelDetail, error) {
	s.modelRefreshMu.Lock()
	defer s.modelRefreshMu.Unlock()

	// 双检锁：加锁后再次检查缓存是否已被其他 goroutine 刷新
	s.modelCacheMu.RLock()
	if len(s.modelCache) > 0 && time.Since(s.modelCacheAt) < modelCacheTTL {
		cached := make([]CopilotModelDetail, len(s.modelCache))
		copy(cached, s.modelCache)
		s.modelCacheMu.RUnlock()
		return cached, nil
	}
	s.modelCacheMu.RUnlock()

	accounts, err := s.accountStore.List()
	if err != nil {
		return nil, fmt.Errorf("列出账户: %w", err)
	}

	for _, acct := range accounts {
		if (acct.Status != nosql.AccountStatusActive && acct.Status != nosql.AccountStatusQuotaExceeded) || acct.OAuthToken == "" {
			continue
		}

		token, err := s.tokenManager.EnsureValidToken(ctx, &acct, s.accountStore)
		if err != nil {
			s.logger.Warn("刷新模型缓存：获取 token 失败，尝试下一个账户",
				"account_id", acct.ID, "error", err)
			continue
		}

		// 重新读取以获取更新后的 APIBaseURL
		refreshed, err := s.accountStore.Get(acct.ID)
		if err != nil {
			refreshed = &acct
		}

		modelsURL := CopilotModelsURL
		if refreshed.APIBaseURL != "" {
			modelsURL = strings.TrimRight(refreshed.APIBaseURL, "/") + "/models"
		}

		details, err := FetchModelDetails(ctx, s.httpClient, token, modelsURL)
		if err != nil {
			s.logger.Warn("刷新模型缓存：获取模型列表失败，尝试下一个账户",
				"account_id", acct.ID, "models_url", modelsURL, "error", err)
			continue
		}

		s.modelCacheMu.Lock()
		s.modelCache = details
		s.modelCacheAt = time.Now()
		s.modelCacheMu.Unlock()

		s.logger.Info("模型缓存已刷新", "count", len(details))
		return details, nil
	}

	// 所有账户都失败了，尝试返回旧缓存
	s.modelCacheMu.RLock()
	defer s.modelCacheMu.RUnlock()
	if len(s.modelCache) > 0 {
		s.logger.Warn("所有账户刷新失败，返回过期缓存", "cache_age", time.Since(s.modelCacheAt))
		cached := make([]CopilotModelDetail, len(s.modelCache))
		copy(cached, s.modelCache)
		return cached, nil
	}

	return nil, fmt.Errorf("无可用账户刷新模型缓存")
}
