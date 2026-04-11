package copilot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestStartDeviceFlow(t *testing.T) {
	expected := DeviceCodeResponse{
		DeviceCode:      "test-device-code",
		UserCode:        "ABCD-1234",
		VerificationURI: "https://github.com/login/device",
		ExpiresIn:       900,
		Interval:        5,
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("期望 POST 方法，得到 %s", r.Method)
		}
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("期望 Accept: application/json，得到 %s", r.Header.Get("Accept"))
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("解析表单: %v", err)
		}
		if r.FormValue("client_id") != GitHubClientID {
			t.Errorf("client_id = %q, 期望 %q", r.FormValue("client_id"), GitHubClientID)
		}
		if r.FormValue("scope") != GitHubScope {
			t.Errorf("scope = %q, 期望 %q", r.FormValue("scope"), GitHubScope)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(expected)
	}))
	defer server.Close()

	client := NewOAuthClient(server.Client(), server.URL, "")
	resp, err := client.StartDeviceFlow(context.Background())
	if err != nil {
		t.Fatalf("StartDeviceFlow: %v", err)
	}

	if resp.DeviceCode != expected.DeviceCode {
		t.Errorf("DeviceCode = %q, 期望 %q", resp.DeviceCode, expected.DeviceCode)
	}
	if resp.UserCode != expected.UserCode {
		t.Errorf("UserCode = %q, 期望 %q", resp.UserCode, expected.UserCode)
	}
	if resp.VerificationURI != expected.VerificationURI {
		t.Errorf("VerificationURI = %q, 期望 %q", resp.VerificationURI, expected.VerificationURI)
	}
	if resp.ExpiresIn != expected.ExpiresIn {
		t.Errorf("ExpiresIn = %d, 期望 %d", resp.ExpiresIn, expected.ExpiresIn)
	}
	if resp.Interval != expected.Interval {
		t.Errorf("Interval = %d, 期望 %d", resp.Interval, expected.Interval)
	}
}

func TestPollForToken_Success(t *testing.T) {
	var callCount int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&callCount, 1)
		w.Header().Set("Content-Type", "application/json")

		if count <= 2 {
			// 前两次返回 authorization_pending
			json.NewEncoder(w).Encode(OAuthTokenResponse{
				Error: "authorization_pending",
			})
			return
		}

		// 第三次返回成功
		json.NewEncoder(w).Encode(OAuthTokenResponse{
			AccessToken: "gho_test_token_123",
			TokenType:   "bearer",
			Scope:       "read:user",
		})
	}))
	defer server.Close()

	client := NewOAuthClient(server.Client(), "", server.URL)
	// 使用 interval=0 使测试快速完成（实际会被设置为最小 1 秒）
	// 由于测试用 interval 会导致等待，这里用短 context
	ctx := context.Background()
	token, err := client.PollForToken(ctx, "test-device-code", 1)
	if err != nil {
		t.Fatalf("PollForToken: %v", err)
	}

	if token != "gho_test_token_123" {
		t.Errorf("token = %q, 期望 %q", token, "gho_test_token_123")
	}
	if atomic.LoadInt32(&callCount) != 3 {
		t.Errorf("调用次数 = %d, 期望 3", atomic.LoadInt32(&callCount))
	}
}

func TestPollForToken_Expired(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(OAuthTokenResponse{
			Error: "expired_token",
		})
	}))
	defer server.Close()

	client := NewOAuthClient(server.Client(), "", server.URL)
	_, err := client.PollForToken(context.Background(), "test-device-code", 1)
	if err != ErrDeviceCodeExpired {
		t.Errorf("期望 ErrDeviceCodeExpired，得到 %v", err)
	}
}

func TestPollForToken_Denied(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(OAuthTokenResponse{
			Error: "access_denied",
		})
	}))
	defer server.Close()

	client := NewOAuthClient(server.Client(), "", server.URL)
	_, err := client.PollForToken(context.Background(), "test-device-code", 1)
	if err != ErrAccessDenied {
		t.Errorf("期望 ErrAccessDenied，得到 %v", err)
	}
}

func TestPollForToken_SlowDown(t *testing.T) {
	var callCount int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&callCount, 1)
		w.Header().Set("Content-Type", "application/json")

		if count == 1 {
			// 第一次返回 slow_down
			json.NewEncoder(w).Encode(OAuthTokenResponse{
				Error: "slow_down",
			})
			return
		}

		// 第二次返回成功
		json.NewEncoder(w).Encode(OAuthTokenResponse{
			AccessToken: "gho_slow_down_token",
			TokenType:   "bearer",
			Scope:       "read:user",
		})
	}))
	defer server.Close()

	client := NewOAuthClient(server.Client(), "", server.URL)
	token, err := client.PollForToken(context.Background(), "test-device-code", 1)
	if err != nil {
		t.Fatalf("PollForToken: %v", err)
	}

	if token != "gho_slow_down_token" {
		t.Errorf("token = %q, 期望 %q", token, "gho_slow_down_token")
	}

	// slow_down 后 interval 应增加 5（从 1 到 6），但我们只验证最终成功
	if atomic.LoadInt32(&callCount) != 2 {
		t.Errorf("调用次数 = %d, 期望 2", atomic.LoadInt32(&callCount))
	}
}

func TestPollForToken_ContextCanceled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(OAuthTokenResponse{
			Error: "authorization_pending",
		})
	}))
	defer server.Close()

	client := NewOAuthClient(server.Client(), "", server.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立即取消

	_, err := client.PollForToken(ctx, "test-device-code", 1)
	if err == nil {
		t.Fatal("期望 context canceled 错误")
	}
}
