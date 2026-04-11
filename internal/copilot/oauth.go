package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 错误变量
var (
	ErrDeviceCodeExpired = errors.New("device code expired")
	ErrAccessDenied      = errors.New("user denied access")
)

// DeviceCodeResponse 表示 device code 请求的响应。
type DeviceCodeResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// OAuthTokenResponse 表示 OAuth token 请求的响应。
type OAuthTokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	Scope       string `json:"scope"`
	Error       string `json:"error,omitempty"`
}

// OAuthClient 封装 OAuth 相关的 HTTP 操作。
type OAuthClient struct {
	httpClient     *http.Client
	deviceCodeURL  string // 可测试替换
	accessTokenURL string // 可测试替换
}

// NewOAuthClient 创建 OAuth 客户端。URL 参数为空时使用默认值。
func NewOAuthClient(httpClient *http.Client, deviceCodeURL, accessTokenURL string) *OAuthClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if deviceCodeURL == "" {
		deviceCodeURL = GitHubDeviceCodeURL
	}
	if accessTokenURL == "" {
		accessTokenURL = GitHubAccessTokenURL
	}
	return &OAuthClient{
		httpClient:     httpClient,
		deviceCodeURL:  deviceCodeURL,
		accessTokenURL: accessTokenURL,
	}
}

// StartDeviceFlow 发起 Device Flow 授权。
// POST deviceCodeURL，body: client_id=...&scope=read:user，Accept: application/json
func (c *OAuthClient) StartDeviceFlow(ctx context.Context) (*DeviceCodeResponse, error) {
	form := url.Values{
		"client_id": {GitHubClientID},
		"scope":     {GitHubScope},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.deviceCodeURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("创建 device code 请求: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("发送 device code 请求: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取 device code 响应: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("device code 请求失败: status=%d, body=%s", resp.StatusCode, string(body))
	}

	var result DeviceCodeResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("解析 device code 响应: %w", err)
	}

	return &result, nil
}

// PollForToken 轮询获取 OAuth token。
// POST accessTokenURL，body: client_id=...&device_code=...&grant_type=urn:ietf:params:oauth:grant-type:device_code
// Accept: application/json
// 处理返回码：
//   - authorization_pending → 等待 interval 后重试
//   - slow_down → interval += 5 后重试
//   - expired_token → 返回 ErrDeviceCodeExpired
//   - access_denied → 返回 ErrAccessDenied
//   - 无 error + 有 access_token → 返回 token
func (c *OAuthClient) PollForToken(ctx context.Context, deviceCode string, interval int) (string, error) {
	if interval <= 0 {
		interval = 5
	}

	for {
		// 检查 ctx 是否已取消
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}

		token, retry, newInterval, err := c.pollOnce(ctx, deviceCode, interval)
		if err != nil {
			return "", err
		}
		if !retry {
			return token, nil
		}

		interval = newInterval

		// 等待 interval 秒后重试，同时检查 ctx
		timer := time.NewTimer(time.Duration(interval) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-timer.C:
		}
	}
}

// pollOnce 执行一次 token 轮询请求。
// 返回值：token, 是否需要重试, 新的 interval, 错误
func (c *OAuthClient) pollOnce(ctx context.Context, deviceCode string, interval int) (string, bool, int, error) {
	form := url.Values{
		"client_id":   {GitHubClientID},
		"device_code": {deviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.accessTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", false, interval, fmt.Errorf("创建 token 请求: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", false, interval, fmt.Errorf("发送 token 请求: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", false, interval, fmt.Errorf("读取 token 响应: %w", err)
	}

	var result OAuthTokenResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return "", false, interval, fmt.Errorf("解析 token 响应: %w", err)
	}

	switch result.Error {
	case "":
		// 无错误，检查是否有 access_token
		if result.AccessToken != "" {
			return result.AccessToken, false, interval, nil
		}
		return "", false, interval, fmt.Errorf("响应中无 access_token 且无 error")
	case "authorization_pending":
		return "", true, interval, nil
	case "slow_down":
		return "", true, interval + 5, nil
	case "expired_token":
		return "", false, interval, ErrDeviceCodeExpired
	case "access_denied":
		return "", false, interval, ErrAccessDenied
	default:
		return "", false, interval, fmt.Errorf("未知 OAuth 错误: %s", result.Error)
	}
}
