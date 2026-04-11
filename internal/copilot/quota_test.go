package copilot

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ycgame/llms-proxy/internal/nosql"
)

func TestSyncQuotaFromGitHub(t *testing.T) {
	// 构造正常响应
	response := gitHubCopilotUserResponse{
		CopilotPlan: "individual",
	}
	response.QuotaSnapshots.PremiumInteractions.PercentRemaining = 85.5

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 验证请求头
		if got := r.Header.Get("Authorization"); got != "token test-oauth-token" {
			t.Errorf("Authorization = %q, want %q", got, "token test-oauth-token")
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q, want %q", got, "application/json")
		}
		if got := r.Header.Get("Editor-Version"); got != "vscode/1.96.2" {
			t.Errorf("Editor-Version = %q, want %q", got, "vscode/1.96.2")
		}
		if got := r.Header.Get("Editor-Plugin-Version"); got != "copilot/1.254.0" {
			t.Errorf("Editor-Plugin-Version = %q, want %q", got, "copilot/1.254.0")
		}
		if got := r.Header.Get("User-Agent"); got != "GitHubCopilotChat/0.24.2024" {
			t.Errorf("User-Agent = %q, want %q", got, "GitHubCopilotChat/0.24.2024")
		}
		if got := r.Header.Get("Copilot-Integration-Id"); got != "vscode-chat" {
			t.Errorf("Copilot-Integration-Id = %q, want %q", got, "vscode-chat")
		}
		if r.Method != http.MethodGet {
			t.Errorf("Method = %q, want %q", r.Method, http.MethodGet)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	mgr := NewQuotaManager(server.Client(), server.URL, nil)
	info, err := mgr.SyncQuotaFromGitHub(context.Background(), "test-oauth-token")
	if err != nil {
		t.Fatalf("SyncQuotaFromGitHub() error = %v", err)
	}

	if info.PercentRemaining != 85.5 {
		t.Errorf("PercentRemaining = %v, want 85.5", info.PercentRemaining)
	}
	if info.CopilotPlan != "individual" {
		t.Errorf("CopilotPlan = %q, want %q", info.CopilotPlan, "individual")
	}
}

func TestSyncQuotaFromGitHub_Error(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
	}{
		{
			name:       "401 未授权",
			statusCode: http.StatusUnauthorized,
			body:       `{"message":"Bad credentials"}`,
		},
		{
			name:       "500 服务器错误",
			statusCode: http.StatusInternalServerError,
			body:       `{"message":"Internal Server Error"}`,
		},
		{
			name:       "403 禁止访问",
			statusCode: http.StatusForbidden,
			body:       `{"message":"Forbidden"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
				w.Write([]byte(tt.body))
			}))
			defer server.Close()

			mgr := NewQuotaManager(server.Client(), server.URL, nil)
			_, err := mgr.SyncQuotaFromGitHub(context.Background(), "bad-token")
			if err == nil {
				t.Fatal("SyncQuotaFromGitHub() 期望返回错误, 但没有")
			}
		})
	}
}

func TestSyncQuotaFromGitHub_InvalidJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("invalid json"))
	}))
	defer server.Close()

	mgr := NewQuotaManager(server.Client(), server.URL, nil)
	_, err := mgr.SyncQuotaFromGitHub(context.Background(), "test-token")
	if err == nil {
		t.Fatal("SyncQuotaFromGitHub() 期望返回 JSON 解析错误, 但没有")
	}
}

func TestDeductQuota(t *testing.T) {
	tests := []struct {
		name              string
		model             string
		initialPercent    float64
		expectedPercent   float64
		shouldBeUnchanged bool
	}{
		{
			name:              "免费模型不扣减",
			model:             "gpt-4o",
			initialPercent:    100.0,
			expectedPercent:   100.0,
			shouldBeUnchanged: true,
		},
		{
			name:           "标准模型扣减（乘数 1）",
			model:          "claude-sonnet-4",
			initialPercent: 100.0,
			// 扣减量 = (1 / 300) * 100 ≈ 0.3333
			expectedPercent: 100.0 - (1.0/DefaultMonthlyPremiumRequests)*100,
		},
		{
			name:           "高消耗模型扣减（乘数 3）",
			model:          "claude-opus-4.5",
			initialPercent: 100.0,
			// 扣减量 = (3 / 300) * 100 = 1.0
			expectedPercent: 100.0 - (3.0/DefaultMonthlyPremiumRequests)*100,
		},
		{
			name:           "低消耗模型扣减（乘数 0.33）",
			model:          "claude-haiku-4.5",
			initialPercent: 50.0,
			// 扣减量 = (0.33 / 300) * 100 = 0.11
			expectedPercent: 50.0 - (0.33/DefaultMonthlyPremiumRequests)*100,
		},
		{
			name:           "扣减后不低于 0",
			model:          "claude-opus-4.5",
			initialPercent: 0.5,
			// 扣减量 = (3 / 300) * 100 = 1.0，0.5 - 1.0 = -0.5 → 0
			expectedPercent: 0.0,
		},
		{
			name:            "已经是 0 不变负",
			model:           "claude-sonnet-4",
			initialPercent:  0.0,
			expectedPercent: 0.0,
		},
		{
			name:              "带 copilot_ 前缀",
			model:             "copilot_gpt-4o",
			initialPercent:    80.0,
			expectedPercent:   80.0,
			shouldBeUnchanged: true,
		},
		{
			name:            "未知模型默认乘数 1.0",
			model:           "unknown-model",
			initialPercent:  50.0,
			expectedPercent: 50.0 - (1.0/DefaultMonthlyPremiumRequests)*100,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account := &nosql.CopilotAccount{
				QuotaPercentRemaining: tt.initialPercent,
			}

			DeductQuota(account, tt.model)

			if tt.shouldBeUnchanged {
				if account.QuotaPercentRemaining != tt.initialPercent {
					t.Errorf("额度不应变化: got %v, want %v",
						account.QuotaPercentRemaining, tt.initialPercent)
				}
				return
			}

			// 使用小误差比较浮点数
			if math.Abs(account.QuotaPercentRemaining-tt.expectedPercent) > 1e-9 {
				t.Errorf("额度 = %v, 期望 %v", account.QuotaPercentRemaining, tt.expectedPercent)
			}

			// 验证不低于 0
			if account.QuotaPercentRemaining < 0 {
				t.Errorf("额度不应低于 0, got %v", account.QuotaPercentRemaining)
			}
		})
	}
}

func TestIsQuotaExhausted(t *testing.T) {
	tests := []struct {
		name    string
		percent float64
		want    bool
	}{
		{name: "有额度", percent: 50.0, want: false},
		{name: "满额度", percent: 100.0, want: false},
		{name: "少量额度", percent: 0.01, want: false},
		{name: "零额度", percent: 0.0, want: true},
		{name: "负额度（不应出现但需处理）", percent: -1.0, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account := &nosql.CopilotAccount{
				QuotaPercentRemaining: tt.percent,
			}
			got := IsQuotaExhausted(account)
			if got != tt.want {
				t.Errorf("IsQuotaExhausted(percent=%v) = %v, want %v", tt.percent, got, tt.want)
			}
		})
	}
}

func TestNewQuotaManager_Defaults(t *testing.T) {
	mgr := NewQuotaManager(nil, "", nil)
	if mgr.httpClient == nil {
		t.Error("httpClient 不应为 nil")
	}
	if mgr.quotaURL != defaultQuotaURL {
		t.Errorf("quotaURL = %q, want %q", mgr.quotaURL, defaultQuotaURL)
	}
	if mgr.logger == nil {
		t.Error("logger 不应为 nil")
	}
}

func TestQuotaManager_Stop(t *testing.T) {
	mgr := NewQuotaManager(nil, "", nil)

	// 首次 Stop 不应 panic
	mgr.Stop()

	// 二次 Stop 也不应 panic
	mgr.Stop()

	if !mgr.stopped {
		t.Error("stopped 应为 true")
	}
}
