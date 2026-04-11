package copilot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/ycgame/llms-proxy/internal/nosql"
)

func TestFetchCopilotToken(t *testing.T) {
	expected := CopilotTokenResponse{
		Token:     "tid=test-copilot-token",
		ExpiresAt: time.Now().Add(30 * time.Minute).Unix(),
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("期望 GET 方法，得到 %s", r.Method)
		}
		// 验证 Authorization header
		auth := r.Header.Get("Authorization")
		if auth != "token gho_test_oauth" {
			t.Errorf("Authorization = %q, 期望 %q", auth, "token gho_test_oauth")
		}
		// 验证 Editor Headers
		if r.Header.Get("Editor-Version") != HeaderEditorVersion {
			t.Errorf("Editor-Version = %q, 期望 %q", r.Header.Get("Editor-Version"), HeaderEditorVersion)
		}
		if r.Header.Get("Editor-Plugin-Version") != HeaderPluginVersion {
			t.Errorf("Editor-Plugin-Version = %q, 期望 %q", r.Header.Get("Editor-Plugin-Version"), HeaderPluginVersion)
		}
		if r.Header.Get("User-Agent") != HeaderUserAgent {
			t.Errorf("User-Agent = %q, 期望 %q", r.Header.Get("User-Agent"), HeaderUserAgent)
		}
		if r.Header.Get("Copilot-Integration-Id") != HeaderIntegrationID {
			t.Errorf("Copilot-Integration-Id = %q, 期望 %q", r.Header.Get("Copilot-Integration-Id"), HeaderIntegrationID)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(expected)
	}))
	defer server.Close()

	tm := NewTokenManager(server.Client(), server.URL)
	resp, err := tm.FetchCopilotToken(context.Background(), "gho_test_oauth")
	if err != nil {
		t.Fatalf("FetchCopilotToken: %v", err)
	}

	if resp.Token != expected.Token {
		t.Errorf("Token = %q, 期望 %q", resp.Token, expected.Token)
	}
	if resp.ExpiresAt != expected.ExpiresAt {
		t.Errorf("ExpiresAt = %d, 期望 %d", resp.ExpiresAt, expected.ExpiresAt)
	}
}

// testDB 是创建测试用 bbolt 数据库的辅助函数。
func testDB(t *testing.T) *nosql.CopilotAccountStore {
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

	return accountStore
}

func TestEnsureValidToken_NotExpired(t *testing.T) {
	accountStore := testDB(t)

	// 创建一个有有效 token 的账户
	err := accountStore.Create(nosql.CopilotAccount{
		PoolName:              "test-pool",
		Status:                nosql.AccountStatusActive,
		OAuthToken:            "gho_test",
		CopilotToken:          "valid-token",
		CopilotTokenExpiresAt: time.Now().Add(30 * time.Minute).Unix(), // 30 分钟后过期
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
	account := accounts[0]

	// Token 未过期，不应调用 HTTP
	tm := NewTokenManager(nil, "http://should-not-be-called")
	token, err := tm.EnsureValidToken(context.Background(), &account, accountStore)
	if err != nil {
		t.Fatalf("EnsureValidToken: %v", err)
	}

	if token != "valid-token" {
		t.Errorf("token = %q, 期望 %q", token, "valid-token")
	}
}

func TestEnsureValidToken_Refresh(t *testing.T) {
	accountStore := testDB(t)

	newToken := CopilotTokenResponse{
		Token:     "refreshed-copilot-token",
		ExpiresAt: time.Now().Add(30 * time.Minute).Unix(),
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(newToken)
	}))
	defer server.Close()

	// 创建一个 token 已过期的账户
	err := accountStore.Create(nosql.CopilotAccount{
		PoolName:              "test-pool",
		Status:                nosql.AccountStatusActive,
		OAuthToken:            "gho_test_refresh",
		CopilotToken:          "expired-token",
		CopilotTokenExpiresAt: time.Now().Add(-10 * time.Minute).Unix(), // 已过期
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
	account := accounts[0]

	tm := NewTokenManager(server.Client(), server.URL)
	token, err := tm.EnsureValidToken(context.Background(), &account, accountStore)
	if err != nil {
		t.Fatalf("EnsureValidToken: %v", err)
	}

	if token != newToken.Token {
		t.Errorf("token = %q, 期望 %q", token, newToken.Token)
	}

	// 验证 store 中的 token 也已更新
	updated, err := accountStore.Get(account.ID)
	if err != nil {
		t.Fatalf("获取更新后的账户: %v", err)
	}
	if updated.CopilotToken != newToken.Token {
		t.Errorf("存储中的 token = %q, 期望 %q", updated.CopilotToken, newToken.Token)
	}
	if updated.CopilotTokenExpiresAt != newToken.ExpiresAt {
		t.Errorf("存储中的 ExpiresAt = %d, 期望 %d", updated.CopilotTokenExpiresAt, newToken.ExpiresAt)
	}
}
