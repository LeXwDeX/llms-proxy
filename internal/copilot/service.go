package copilot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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
