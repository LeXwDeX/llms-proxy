package proxy

import (
	"encoding/json"
	"path/filepath"
	"testing"

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

	// 创建两个 active 账户，SortOrder 2 先创建（自动 SortOrder=1），SortOrder 1 后创建（自动=2）
	// 然后手动设置 SortOrder
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

	// 使用付费模型（copilot_claude-sonnet-4，乘数=1）
	got, err := copilotSvc.SelectAccount("pool1", "copilot_claude-sonnet-4")
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

	// 账户1：额度耗尽（active 但 quota=0）
	acct1 := nosql.CopilotAccount{
		PoolName:              "pool1",
		GitHubUsername:        "user-exhausted",
		Status:                nosql.AccountStatusActive,
		SortOrder:             1,
		QuotaPercentRemaining: 0,
	}
	// 账户2：有额度
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
	got, err := copilotSvc.SelectAccount("pool1", "copilot_claude-sonnet-4")
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

	// 唯一账户：quota_exceeded 状态，额度为 0
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

	// 免费模型（copilot_gpt-4o，乘数=0）应该不受额度限制
	got, err := copilotSvc.SelectAccount("pool1", "copilot_gpt-4o")
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

	// 唯一账户：disabled
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

	_, err := copilotSvc.SelectAccount("pool1", "copilot_gpt-4o")
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

	_, err := copilotSvc.SelectAccount("pool1", "copilot_gpt-4o")
	if err == nil {
		t.Fatal("expected error for empty pool")
	}
}

// ---------- replaceModelInBody ----------

func TestReplaceModelInBody_Normal(t *testing.T) {
	body := []byte(`{"model":"copilot_gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	result := replaceModelInBody(body, "gpt-4o")

	var parsed map[string]interface{}
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if parsed["model"] != "gpt-4o" {
		t.Fatalf("expected model gpt-4o, got %v", parsed["model"])
	}
	// messages 应保留
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
	// 没有 model 字段，不应添加
	if _, ok := parsed["model"]; ok {
		t.Fatal("model field should not be added when not present")
	}
}

func TestReplaceModelInBody_InvalidJSON(t *testing.T) {
	body := []byte(`not json at all`)
	result := replaceModelInBody(body, "gpt-4o")

	// 应返回原始 body
	if string(result) != string(body) {
		t.Fatalf("expected original body for invalid JSON, got %q", string(result))
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
