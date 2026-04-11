package nosql

import (
	"strings"
	"testing"
)

// createTestPool 是创建测试用 Pool 的辅助函数。
func createTestPool(t *testing.T, store *CopilotPoolStore, name, clientName string) {
	t.Helper()
	err := store.Create(CopilotPool{
		Name:       name,
		ClientName: clientName,
	})
	if err != nil {
		t.Fatalf("create test pool %q: %v", name, err)
	}
}

func TestCopilotAccountStore_CRUD(t *testing.T) {
	db := testDB(t)
	poolStore := NewCopilotPoolStore(db)
	accountStore := NewCopilotAccountStore(db, poolStore)

	// 先创建 Pool
	createTestPool(t, poolStore, "pool-1", "client-1")

	// Create
	err := accountStore.Create(CopilotAccount{
		PoolName: "pool-1",
		Status:   AccountStatusActive,
		Notes:    "test account",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// List
	accounts, err := accountStore.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("expected 1 account, got %d", len(accounts))
	}
	a := accounts[0]
	if a.ID == "" {
		t.Error("expected ID to be auto-generated")
	}
	if a.PoolName != "pool-1" {
		t.Errorf("pool_name = %q, want %q", a.PoolName, "pool-1")
	}
	if a.Status != AccountStatusActive {
		t.Errorf("status = %q, want %q", a.Status, AccountStatusActive)
	}
	if a.SortOrder != 1 {
		t.Errorf("sort_order = %d, want 1", a.SortOrder)
	}
	if a.CreatedAt == "" {
		t.Error("expected CreatedAt to be set")
	}
	if a.UpdatedAt == "" {
		t.Error("expected UpdatedAt to be set")
	}

	// Get
	got, err := accountStore.Get(a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != a.ID || got.PoolName != "pool-1" {
		t.Fatalf("get returned unexpected account: %+v", got)
	}

	// Update
	got.Notes = "updated notes"
	got.GitHubUsername = "testuser"
	err = accountStore.Update(a.ID, *got)
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	updated, err := accountStore.Get(a.ID)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if updated.Notes != "updated notes" {
		t.Errorf("notes = %q, want %q", updated.Notes, "updated notes")
	}
	if updated.GitHubUsername != "testuser" {
		t.Errorf("github_username = %q, want %q", updated.GitHubUsername, "testuser")
	}
	if updated.ID != a.ID {
		t.Error("ID should be immutable after update")
	}

	// Delete
	err = accountStore.Delete(a.ID)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}

	accounts, err = accountStore.List()
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(accounts) != 0 {
		t.Fatalf("expected 0 accounts, got %d", len(accounts))
	}

	// Get deleted — should fail
	_, err = accountStore.Get(a.ID)
	if err == nil {
		t.Fatal("expected error getting deleted account")
	}
}

func TestCopilotAccountStore_ListByPool(t *testing.T) {
	db := testDB(t)
	poolStore := NewCopilotPoolStore(db)
	accountStore := NewCopilotAccountStore(db, poolStore)

	createTestPool(t, poolStore, "pool-a", "client-a")
	createTestPool(t, poolStore, "pool-b", "client-b")

	// 在 pool-a 中创建 3 个账户，指定不同的 SortOrder
	for _, so := range []int{3, 1, 2} {
		err := accountStore.Create(CopilotAccount{
			PoolName:  "pool-a",
			SortOrder: so,
			Status:    AccountStatusActive,
		})
		if err != nil {
			t.Fatalf("create account with sort_order %d: %v", so, err)
		}
	}

	// 在 pool-b 中创建 1 个账户
	err := accountStore.Create(CopilotAccount{
		PoolName: "pool-b",
		Status:   AccountStatusActive,
	})
	if err != nil {
		t.Fatalf("create account in pool-b: %v", err)
	}

	// ListByPool("pool-a") 应返回 3 个，按 SortOrder 升序
	accounts, err := accountStore.ListByPool("pool-a")
	if err != nil {
		t.Fatalf("list by pool: %v", err)
	}
	if len(accounts) != 3 {
		t.Fatalf("expected 3 accounts, got %d", len(accounts))
	}
	for i := 0; i < len(accounts)-1; i++ {
		if accounts[i].SortOrder > accounts[i+1].SortOrder {
			t.Errorf("accounts not sorted: sort_order[%d]=%d > sort_order[%d]=%d",
				i, accounts[i].SortOrder, i+1, accounts[i+1].SortOrder)
		}
	}

	// ListByPool("pool-b") 应返回 1 个
	accountsB, err := accountStore.ListByPool("pool-b")
	if err != nil {
		t.Fatalf("list by pool-b: %v", err)
	}
	if len(accountsB) != 1 {
		t.Fatalf("expected 1 account in pool-b, got %d", len(accountsB))
	}
}

func TestCopilotAccountStore_MaxAccounts(t *testing.T) {
	db := testDB(t)
	poolStore := NewCopilotPoolStore(db)
	accountStore := NewCopilotAccountStore(db, poolStore)

	// 创建一个 MaxAccounts=2 的 Pool
	err := poolStore.Create(CopilotPool{
		Name:        "limited-pool",
		ClientName:  "client-limited",
		MaxAccounts: 2,
	})
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}

	// 创建第 1 个账户 — 应成功
	err = accountStore.Create(CopilotAccount{PoolName: "limited-pool"})
	if err != nil {
		t.Fatalf("create account 1: %v", err)
	}

	// 创建第 2 个账户 — 应成功
	err = accountStore.Create(CopilotAccount{PoolName: "limited-pool"})
	if err != nil {
		t.Fatalf("create account 2: %v", err)
	}

	// 创建第 3 个账户 — 应失败（超过 MaxAccounts=2）
	err = accountStore.Create(CopilotAccount{PoolName: "limited-pool"})
	if err == nil {
		t.Fatal("expected error when exceeding max accounts")
	}
	if !strings.Contains(err.Error(), "max") {
		t.Errorf("error message should mention max, got: %v", err)
	}
}

func TestCopilotAccountStore_AutoSortOrder(t *testing.T) {
	db := testDB(t)
	poolStore := NewCopilotPoolStore(db)
	accountStore := NewCopilotAccountStore(db, poolStore)

	createTestPool(t, poolStore, "pool-auto", "client-auto")

	// 创建第 1 个账户（SortOrder 未指定，应自动设为 1）
	err := accountStore.Create(CopilotAccount{PoolName: "pool-auto"})
	if err != nil {
		t.Fatalf("create account 1: %v", err)
	}

	// 创建第 2 个账户（SortOrder 未指定，应自动设为 2）
	err = accountStore.Create(CopilotAccount{PoolName: "pool-auto"})
	if err != nil {
		t.Fatalf("create account 2: %v", err)
	}

	// 创建第 3 个账户（SortOrder 手动指定为 10）
	err = accountStore.Create(CopilotAccount{PoolName: "pool-auto", SortOrder: 10})
	if err != nil {
		t.Fatalf("create account 3: %v", err)
	}

	// 创建第 4 个账户（SortOrder 未指定，应自动设为 11）
	err = accountStore.Create(CopilotAccount{PoolName: "pool-auto"})
	if err != nil {
		t.Fatalf("create account 4: %v", err)
	}

	accounts, err := accountStore.ListByPool("pool-auto")
	if err != nil {
		t.Fatalf("list by pool: %v", err)
	}
	if len(accounts) != 4 {
		t.Fatalf("expected 4 accounts, got %d", len(accounts))
	}

	// 验证排序后的 SortOrder: 1, 2, 10, 11
	expectedOrders := []int{1, 2, 10, 11}
	for i, expected := range expectedOrders {
		if accounts[i].SortOrder != expected {
			t.Errorf("accounts[%d].SortOrder = %d, want %d", i, accounts[i].SortOrder, expected)
		}
	}
}

func TestCopilotAccountStore_PoolNotFound(t *testing.T) {
	db := testDB(t)
	poolStore := NewCopilotPoolStore(db)
	accountStore := NewCopilotAccountStore(db, poolStore)

	// 未创建任何 Pool，直接创建 Account 应失败
	err := accountStore.Create(CopilotAccount{PoolName: "nonexistent-pool"})
	if err == nil {
		t.Fatal("expected error when pool not found")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error message should mention not found, got: %v", err)
	}
}

func TestCopilotAccountStore_DefaultStatus(t *testing.T) {
	db := testDB(t)
	poolStore := NewCopilotPoolStore(db)
	accountStore := NewCopilotAccountStore(db, poolStore)

	createTestPool(t, poolStore, "pool-status", "client-status")

	// 创建账户时不指定 Status
	err := accountStore.Create(CopilotAccount{PoolName: "pool-status"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	accounts, err := accountStore.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("expected 1 account, got %d", len(accounts))
	}
	if accounts[0].Status != AccountStatusPendingAuth {
		t.Errorf("status = %q, want %q", accounts[0].Status, AccountStatusPendingAuth)
	}
}

func TestCopilotPool_GetMaxAccounts(t *testing.T) {
	// 默认值（MaxAccounts 为 0）
	p := &CopilotPool{Name: "test", ClientName: "c"}
	if got := p.GetMaxAccounts(); got != 5 {
		t.Errorf("GetMaxAccounts() = %d, want 5 (default)", got)
	}

	// 负值时返回默认值
	p.MaxAccounts = -1
	if got := p.GetMaxAccounts(); got != 5 {
		t.Errorf("GetMaxAccounts() = %d, want 5 (negative)", got)
	}

	// 自定义值
	p.MaxAccounts = 10
	if got := p.GetMaxAccounts(); got != 10 {
		t.Errorf("GetMaxAccounts() = %d, want 10", got)
	}

	// 最小正值
	p.MaxAccounts = 1
	if got := p.GetMaxAccounts(); got != 1 {
		t.Errorf("GetMaxAccounts() = %d, want 1", got)
	}
}
