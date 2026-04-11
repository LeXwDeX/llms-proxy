package copilot

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/ycgame/llms-proxy/internal/nosql"
)

// setupServiceTest 创建测试用 CopilotService 和相关 store。
func setupServiceTest(t *testing.T, deviceServer, tokenServer, userServer *httptest.Server) (*CopilotService, *nosql.CopilotAccountStore, *nosql.CopilotPoolStore) {
	t.Helper()
	db, err := nosql.OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	poolStore := nosql.NewCopilotPoolStore(db)
	accountStore := nosql.NewCopilotAccountStore(db, poolStore)

	// 创建测试用 pool
	err = poolStore.Create(nosql.CopilotPool{
		Name:       "test-pool",
		ClientName: "test-client",
	})
	if err != nil {
		t.Fatalf("创建测试 pool: %v", err)
	}

	svc := &CopilotService{
		accountStore: accountStore,
		poolStore:    poolStore,
		logger:       nil,
	}

	// 设置默认 logger
	svc.logger = slog.Default()

	// 设置 OAuth client
	deviceURL := ""
	tokenURL := ""
	if deviceServer != nil {
		deviceURL = deviceServer.URL
	}
	if tokenServer != nil {
		tokenURL = tokenServer.URL
	}
	svc.oauthClient = NewOAuthClient(http.DefaultClient, deviceURL, tokenURL)

	// 设置 token manager
	copilotTokenURL := ""
	if tokenServer != nil {
		copilotTokenURL = tokenServer.URL
	}
	svc.tokenManager = NewTokenManager(http.DefaultClient, copilotTokenURL)

	// 设置 httpClient（用于 fetchGitHubUsername）
	svc.httpClient = http.DefaultClient

	return svc, accountStore, poolStore
}

func TestInitiateAuth(t *testing.T) {
	deviceResp := DeviceCodeResponse{
		DeviceCode:      "test-device-code-init",
		UserCode:        "INIT-1234",
		VerificationURI: "https://github.com/login/device",
		ExpiresIn:       900,
		Interval:        5,
	}

	deviceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(deviceResp)
	}))
	defer deviceServer.Close()

	svc, accountStore, _ := setupServiceTest(t, deviceServer, nil, nil)

	accountID, userCode, verificationURI, err := svc.InitiateAuth(context.Background(), "test-pool")
	if err != nil {
		t.Fatalf("InitiateAuth: %v", err)
	}

	if accountID == "" {
		t.Error("期望非空的 accountID")
	}
	if userCode != deviceResp.UserCode {
		t.Errorf("userCode = %q, 期望 %q", userCode, deviceResp.UserCode)
	}
	if verificationURI != deviceResp.VerificationURI {
		t.Errorf("verificationURI = %q, 期望 %q", verificationURI, deviceResp.VerificationURI)
	}

	// 验证账户已创建
	account, err := accountStore.Get(accountID)
	if err != nil {
		t.Fatalf("获取账户: %v", err)
	}
	if account.Status != nosql.AccountStatusPendingAuth {
		t.Errorf("status = %q, 期望 %q", account.Status, nosql.AccountStatusPendingAuth)
	}
	if account.PoolName != "test-pool" {
		t.Errorf("pool_name = %q, 期望 %q", account.PoolName, "test-pool")
	}
}

func TestGetToken(t *testing.T) {
	copilotToken := CopilotTokenResponse{
		Token:     "test-copilot-token-get",
		ExpiresAt: time.Now().Add(30 * time.Minute).Unix(),
	}

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(copilotToken)
	}))
	defer tokenServer.Close()

	svc, accountStore, _ := setupServiceTest(t, nil, tokenServer, nil)

	// 创建一个 active 账户，token 有效
	err := accountStore.Create(nosql.CopilotAccount{
		PoolName:              "test-pool",
		Status:                nosql.AccountStatusActive,
		OAuthToken:            "gho_test_get",
		CopilotToken:          "valid-copilot-token",
		CopilotTokenExpiresAt: time.Now().Add(30 * time.Minute).Unix(),
	})
	if err != nil {
		t.Fatalf("创建账户: %v", err)
	}

	accounts, err := accountStore.List()
	if err != nil {
		t.Fatalf("列出账户: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("期望 1 个账户，得到 %d", len(accounts))
	}

	// Token 未过期，直接返回
	token, err := svc.GetToken(context.Background(), accounts[0].ID)
	if err != nil {
		t.Fatalf("GetToken: %v", err)
	}
	if token != "valid-copilot-token" {
		t.Errorf("token = %q, 期望 %q", token, "valid-copilot-token")
	}
}

func TestGetToken_Refresh(t *testing.T) {
	newToken := CopilotTokenResponse{
		Token:     "refreshed-copilot-token-svc",
		ExpiresAt: time.Now().Add(30 * time.Minute).Unix(),
	}

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(newToken)
	}))
	defer tokenServer.Close()

	svc, accountStore, _ := setupServiceTest(t, nil, tokenServer, nil)

	// 创建一个 active 账户，token 已过期
	err := accountStore.Create(nosql.CopilotAccount{
		PoolName:              "test-pool",
		Status:                nosql.AccountStatusActive,
		OAuthToken:            "gho_test_refresh_svc",
		CopilotToken:          "expired-token",
		CopilotTokenExpiresAt: time.Now().Add(-10 * time.Minute).Unix(),
	})
	if err != nil {
		t.Fatalf("创建账户: %v", err)
	}

	accounts, err := accountStore.List()
	if err != nil {
		t.Fatalf("列出账户: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("期望 1 个账户，得到 %d", len(accounts))
	}

	token, err := svc.GetToken(context.Background(), accounts[0].ID)
	if err != nil {
		t.Fatalf("GetToken: %v", err)
	}
	if token != newToken.Token {
		t.Errorf("token = %q, 期望 %q", token, newToken.Token)
	}
}

func TestGetToken_NonActiveAccount(t *testing.T) {
	svc, accountStore, _ := setupServiceTest(t, nil, nil, nil)

	// 创建一个 disabled 账户
	err := accountStore.Create(nosql.CopilotAccount{
		PoolName: "test-pool",
		Status:   nosql.AccountStatusDisabled,
	})
	if err != nil {
		t.Fatalf("创建账户: %v", err)
	}

	accounts, err := accountStore.List()
	if err != nil {
		t.Fatalf("列出账户: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("期望 1 个账户，得到 %d", len(accounts))
	}

	_, err = svc.GetToken(context.Background(), accounts[0].ID)
	if err == nil {
		t.Fatal("期望非 active 账户获取 token 时返回错误")
	}
}
