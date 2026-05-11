package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ycgame/llms-proxy/internal/auth"
	"github.com/ycgame/llms-proxy/internal/copilot"
	"github.com/ycgame/llms-proxy/internal/nosql"
)

// ---------- FindPoolByClient ----------

func TestFindPoolByClient_Found(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := nosql.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	poolStore := nosql.NewCopilotPoolStore(db)
	if err := poolStore.Create(nosql.CopilotPool{
		Name:       "pool-alpha",
		ClientName: "ClientA",
	}); err != nil {
		t.Fatalf("create pool: %v", err)
	}

	acctStore := nosql.NewCopilotAccountStore(db, poolStore)
	copilotSvc := copilot.NewCopilotService(acctStore, poolStore, nil, nil)

	pool, err := copilotSvc.FindPoolByClient("clienta") // 大小写不敏感
	if err != nil {
		t.Fatalf("FindPoolByClient: %v", err)
	}
	if pool.Name != "pool-alpha" {
		t.Fatalf("expected pool name pool-alpha, got %q", pool.Name)
	}
}

func TestFindPoolByClient_NotFound(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := nosql.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	poolStore := nosql.NewCopilotPoolStore(db)
	if err := poolStore.Create(nosql.CopilotPool{
		Name:       "pool-alpha",
		ClientName: "ClientA",
	}); err != nil {
		t.Fatalf("create pool: %v", err)
	}

	acctStore := nosql.NewCopilotAccountStore(db, poolStore)
	copilotSvc := copilot.NewCopilotService(acctStore, poolStore, nil, nil)

	_, err = copilotSvc.FindPoolByClient("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent client")
	}
}

// ---------- SelectAccount ----------

func setupTestDB(t *testing.T) (*nosql.CopilotPoolStore, *nosql.CopilotAccountStore) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := nosql.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	poolStore := nosql.NewCopilotPoolStore(db)
	acctStore := nosql.NewCopilotAccountStore(db, poolStore)
	return poolStore, acctStore
}

func TestSelectAccount_OrderBySort(t *testing.T) {
	poolStore, acctStore := setupTestDB(t)
	if err := poolStore.Create(nosql.CopilotPool{
		Name:        "pool1",
		ClientName:  "client1",
		MaxAccounts: 5,
	}); err != nil {
		t.Fatalf("create pool: %v", err)
	}

	acct1 := nosql.CopilotAccount{
		PoolName:              "pool1",
		GitHubUsername:        "user-second",
		Status:                nosql.AccountStatusActive,
		SortOrder:             2,
		QuotaPercentRemaining: 80,
	}
	acct2 := nosql.CopilotAccount{
		PoolName:              "pool1",
		GitHubUsername:        "user-first",
		Status:                nosql.AccountStatusActive,
		SortOrder:             1,
		QuotaPercentRemaining: 50,
	}
	if err := acctStore.Create(acct1); err != nil {
		t.Fatalf("create acct1: %v", err)
	}
	if err := acctStore.Create(acct2); err != nil {
		t.Fatalf("create acct2: %v", err)
	}

	copilotSvc := copilot.NewCopilotService(acctStore, poolStore, nil, nil)

	// 使用付费模型（Copilot claude-sonnet-4，乘数=1）
	got, err := copilotSvc.SelectAccount("pool1", "Copilot claude-sonnet-4")
	if err != nil {
		t.Fatalf("SelectAccount: %v", err)
	}
	// SortOrder=1 的应该被选中
	if got.GitHubUsername != "user-first" {
		t.Fatalf("expected user-first (SortOrder=1), got %q (SortOrder=%d)", got.GitHubUsername, got.SortOrder)
	}
}

func TestSelectAccount_SkipQuotaExhausted(t *testing.T) {
	poolStore, acctStore := setupTestDB(t)
	if err := poolStore.Create(nosql.CopilotPool{
		Name:        "pool1",
		ClientName:  "client1",
		MaxAccounts: 5,
	}); err != nil {
		t.Fatalf("create pool: %v", err)
	}

	acct1 := nosql.CopilotAccount{
		PoolName:              "pool1",
		GitHubUsername:        "user-exhausted",
		Status:                nosql.AccountStatusActive,
		SortOrder:             1,
		QuotaPercentRemaining: 0,
	}
	acct2 := nosql.CopilotAccount{
		PoolName:              "pool1",
		GitHubUsername:        "user-available",
		Status:                nosql.AccountStatusActive,
		SortOrder:             2,
		QuotaPercentRemaining: 60,
	}
	if err := acctStore.Create(acct1); err != nil {
		t.Fatalf("create acct1: %v", err)
	}
	if err := acctStore.Create(acct2); err != nil {
		t.Fatalf("create acct2: %v", err)
	}

	copilotSvc := copilot.NewCopilotService(acctStore, poolStore, nil, nil)

	// 付费模型应跳过额度耗尽的
	got, err := copilotSvc.SelectAccount("pool1", "Copilot claude-sonnet-4")
	if err != nil {
		t.Fatalf("SelectAccount: %v", err)
	}
	if got.GitHubUsername != "user-available" {
		t.Fatalf("expected user-available, got %q", got.GitHubUsername)
	}
}

func TestSelectAccount_FreeModelIgnoresQuota(t *testing.T) {
	poolStore, acctStore := setupTestDB(t)
	if err := poolStore.Create(nosql.CopilotPool{
		Name:        "pool1",
		ClientName:  "client1",
		MaxAccounts: 5,
	}); err != nil {
		t.Fatalf("create pool: %v", err)
	}

	acct := nosql.CopilotAccount{
		PoolName:              "pool1",
		GitHubUsername:        "user-quota-exceeded",
		Status:                nosql.AccountStatusQuotaExceeded,
		SortOrder:             1,
		QuotaPercentRemaining: 0,
	}
	if err := acctStore.Create(acct); err != nil {
		t.Fatalf("create acct: %v", err)
	}

	copilotSvc := copilot.NewCopilotService(acctStore, poolStore, nil, nil)

	// 免费模型（Copilot gpt-4o，乘数=0）应该不受额度限制
	got, err := copilotSvc.SelectAccount("pool1", "Copilot gpt-4o")
	if err != nil {
		t.Fatalf("SelectAccount: %v", err)
	}
	if got.GitHubUsername != "user-quota-exceeded" {
		t.Fatalf("expected user-quota-exceeded, got %q", got.GitHubUsername)
	}
}

func TestSelectAccount_NoAvailable(t *testing.T) {
	poolStore, acctStore := setupTestDB(t)
	if err := poolStore.Create(nosql.CopilotPool{
		Name:        "pool1",
		ClientName:  "client1",
		MaxAccounts: 5,
	}); err != nil {
		t.Fatalf("create pool: %v", err)
	}

	acct := nosql.CopilotAccount{
		PoolName:       "pool1",
		GitHubUsername: "user-disabled",
		Status:         nosql.AccountStatusDisabled,
		SortOrder:      1,
	}
	if err := acctStore.Create(acct); err != nil {
		t.Fatalf("create acct: %v", err)
	}

	copilotSvc := copilot.NewCopilotService(acctStore, poolStore, nil, nil)

	_, err := copilotSvc.SelectAccount("pool1", "Copilot gpt-4o")
	if err == nil {
		t.Fatal("expected error when no accounts available")
	}
}

func TestSelectAccount_EmptyPool(t *testing.T) {
	poolStore, acctStore := setupTestDB(t)
	if err := poolStore.Create(nosql.CopilotPool{
		Name:        "pool1",
		ClientName:  "client1",
		MaxAccounts: 5,
	}); err != nil {
		t.Fatalf("create pool: %v", err)
	}

	copilotSvc := copilot.NewCopilotService(acctStore, poolStore, nil, nil)

	_, err := copilotSvc.SelectAccount("pool1", "Copilot gpt-4o")
	if err == nil {
		t.Fatal("expected error for empty pool")
	}
}

// ---------- replaceModelInBody ----------

func TestReplaceModelInBody_Normal(t *testing.T) {
	body := []byte(`{"model":"Copilot gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	result := replaceModelInBody(body, "gpt-4o")

	var parsed map[string]interface{}
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if parsed["model"] != "gpt-4o" {
		t.Fatalf("expected model gpt-4o, got %v", parsed["model"])
	}
	if _, ok := parsed["messages"]; !ok {
		t.Fatal("messages field lost after replace")
	}
}

func TestReplaceModelInBody_NoModelField(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	result := replaceModelInBody(body, "gpt-4o")

	var parsed map[string]interface{}
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if _, ok := parsed["model"]; ok {
		t.Fatal("model field should not be added when not present")
	}
}

func TestReplaceModelInBody_InvalidJSON(t *testing.T) {
	body := []byte(`not json at all`)
	result := replaceModelInBody(body, "gpt-4o")

	if string(result) != string(body) {
		t.Fatalf("expected original body for invalid JSON, got %q", string(result))
	}
}

func TestReplaceModelInBody_ThinkingSignaturePreserved(t *testing.T) {
	// Simulate an Anthropic Messages API request containing a thinking block
	// with a signature field. The signature is a base64 string that MUST be
	// preserved byte-for-byte; any alteration causes Anthropic to reject the
	// request with "Invalid signature in thinking block".
	body := []byte(`{"model":"Copilot claude-sonnet-4","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"thinking","thinking":"let me think...","signature":"ErUBCkYIAxgCIkD+Q3kD/cBs8G/abc123+/def456==KhBzb21lLXJhbmRvbS1pZBIwc29tZS1yYW5kb20tZXh0cmEtZGF0YS1oZXJl"}]},{"role":"user","content":"continue"},{"role":"assistant","content":[{"type":"thinking","thinking":"more thinking <with> special & chars","signature":"XyZaBcDeFgHiJkLmNoPqRsTuVwXyZ0123456789+/=="}]}]}`)
	result := replaceModelInBody(body, "claude-sonnet-4")

	// Verify byte-for-byte preservation of the signature values.
	if !json.Valid(result) {
		t.Fatalf("result is not valid JSON")
	}

	// Extract signatures from original and result
	type msg struct {
		Content json.RawMessage `json:"content"`
	}
	type req struct {
		Model    string `json:"model"`
		Messages []msg  `json:"messages"`
	}

	var orig, replaced req
	if err := json.Unmarshal(body, &orig); err != nil {
		t.Fatalf("unmarshal original: %v", err)
	}
	if err := json.Unmarshal(result, &replaced); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	if replaced.Model != "claude-sonnet-4" {
		t.Fatalf("model not replaced: got %q", replaced.Model)
	}

	// Compare raw bytes of each message's content to ensure no mutation
	for i, origMsg := range orig.Messages {
		if string(origMsg.Content) != string(replaced.Messages[i].Content) {
			t.Errorf("message[%d] content mutated:\n  orig:    %s\n  replaced:%s", i, origMsg.Content, replaced.Messages[i].Content)
		}
	}
}

func TestReplaceModelInBody_EmptyBody(t *testing.T) {
	result := replaceModelInBody(nil, "gpt-4o")
	if result != nil {
		t.Fatalf("expected nil for empty body, got %q", string(result))
	}

	result = replaceModelInBody([]byte{}, "gpt-4o")
	if len(result) != 0 {
		t.Fatalf("expected empty for empty body, got %q", string(result))
	}
}

// ---------- handleCopilotRequest X-Initiator injection ----------

// TestHandleCopilotRequestInjectsInitiator: the OpenAI-compatible Copilot
// path (handleCopilotRequest, triggered when downstream model has the
// "Copilot " prefix) must also inject X-Initiator into the upstream request.
// Here the last role is "tool" → agent turn.
func TestHandleCopilotRequestInjectsInitiator(t *testing.T) {
	var capturedInitiator string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedInitiator = r.Header.Get("X-Initiator")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	svc := setupPassthroughTestEnv(t, upstream.URL)

	body := []byte(`{"model":"Copilot gpt-4o","messages":[{"role":"user","content":"q"},{"role":"assistant","content":"calling tool"},{"role":"tool","tool_call_id":"abc","content":"result"}]}`)
	r := reqWithPrincipal(t, http.MethodPost, "/chat/completions", strings.NewReader(string(body)), "test-client")
	w := httptest.NewRecorder()

	principal, _ := auth.PrincipalFromContext(r.Context())
	svc.handleCopilotRequest(w, r, principal, body, "Copilot gpt-4o")

	if resp := w.Result(); resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(respBody))
	}
	if capturedInitiator != "agent" {
		t.Fatalf("expected upstream X-Initiator=agent for tool turn, got %q", capturedInitiator)
	}
}
