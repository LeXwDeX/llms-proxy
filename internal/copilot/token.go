package copilot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/ycgame/llms-proxy/internal/nosql"
)

// CopilotTokenResponse 表示 Copilot token 请求的响应。
type CopilotTokenResponse struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expires_at"`
}

// TokenManager 管理 Copilot access token 的获取和刷新。
type TokenManager struct {
	httpClient      *http.Client
	copilotTokenURL string // 可测试替换
	mu              sync.Mutex
	// 每个 accountID 一把锁，防止并发刷新
	refreshLocks map[string]*sync.Mutex
}

// NewTokenManager 创建 Token 管理器。
func NewTokenManager(httpClient *http.Client, copilotTokenURL string) *TokenManager {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if copilotTokenURL == "" {
		copilotTokenURL = CopilotTokenURL
	}
	return &TokenManager{
		httpClient:      httpClient,
		copilotTokenURL: copilotTokenURL,
		refreshLocks:    make(map[string]*sync.Mutex),
	}
}

// getAccountLock 获取指定 accountID 的锁（线程安全）。
func (m *TokenManager) getAccountLock(accountID string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()

	lock, ok := m.refreshLocks[accountID]
	if !ok {
		lock = &sync.Mutex{}
		m.refreshLocks[accountID] = lock
	}
	return lock
}

// FetchCopilotToken 从 GitHub API 获取新的 Copilot access token。
// GET copilotTokenURL
// Authorization: token <oauthToken>（注意：这里用 "token" 前缀，不是 "Bearer"）
// 加上 Editor Headers
func (m *TokenManager) FetchCopilotToken(ctx context.Context, oauthToken string) (*CopilotTokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.copilotTokenURL, nil)
	if err != nil {
		return nil, fmt.Errorf("创建 copilot token 请求: %w", err)
	}
	req.Header.Set("Authorization", "token "+oauthToken)
	ApplyEditorHeaders(req)

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("发送 copilot token 请求: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取 copilot token 响应: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("copilot token 请求失败: status=%d, body=%s", resp.StatusCode, string(body))
	}

	var result CopilotTokenResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("解析 copilot token 响应: %w", err)
	}

	return &result, nil
}

// EnsureValidToken 确保账户有有效的 Copilot access token。
// 如果 token 还有 >5 分钟有效期，直接返回。
// 否则自动刷新并写回 store。
// 使用 per-account 锁防止并发刷新。
func (m *TokenManager) EnsureValidToken(ctx context.Context, account *nosql.CopilotAccount, store *nosql.CopilotAccountStore) (string, error) {
	// 检查当前 token 是否仍然有效（>5 分钟）
	if account.CopilotToken != "" && account.CopilotTokenExpiresAt > time.Now().Unix()+300 {
		return account.CopilotToken, nil
	}

	// 获取 per-account 锁
	lock := m.getAccountLock(account.ID)
	lock.Lock()
	defer lock.Unlock()

	// 双重检查：可能其他 goroutine 已经刷新了
	refreshed, err := store.Get(account.ID)
	if err != nil {
		return "", fmt.Errorf("获取账户 %q: %w", account.ID, err)
	}
	if refreshed.CopilotToken != "" && refreshed.CopilotTokenExpiresAt > time.Now().Unix()+300 {
		// 更新调用者的账户引用
		account.CopilotToken = refreshed.CopilotToken
		account.CopilotTokenExpiresAt = refreshed.CopilotTokenExpiresAt
		return refreshed.CopilotToken, nil
	}

	// 检查是否有 OAuth token
	if refreshed.OAuthToken == "" {
		return "", fmt.Errorf("账户 %q 缺少 OAuth token", account.ID)
	}

	// 刷新 Copilot token
	tokenResp, err := m.FetchCopilotToken(ctx, refreshed.OAuthToken)
	if err != nil {
		return "", fmt.Errorf("刷新 copilot token: %w", err)
	}

	// 写回 store
	refreshed.CopilotToken = tokenResp.Token
	refreshed.CopilotTokenExpiresAt = tokenResp.ExpiresAt
	if err := store.Update(refreshed.ID, *refreshed); err != nil {
		return "", fmt.Errorf("更新账户 copilot token: %w", err)
	}

	// 更新调用者的账户引用
	account.CopilotToken = tokenResp.Token
	account.CopilotTokenExpiresAt = tokenResp.ExpiresAt

	return tokenResp.Token, nil
}
