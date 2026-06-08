package nosql

import (
	"sort"
	"testing"

	"github.com/ycgame/llms-proxy/internal/config"
)

func TestClientStoreListWithQuotaEmpty(t *testing.T) {
	db := testDB(t)
	store := NewClientStore(db)

	clients, err := store.ListWithQuota()
	if err != nil {
		t.Fatalf("ListWithQuota: %v", err)
	}
	if len(clients) != 0 {
		t.Errorf("expected empty list, got %d", len(clients))
	}
	if clients == nil {
		t.Error("expected non-nil empty slice")
	}
}

func TestClientStoreListWithQuotaAllZero(t *testing.T) {
	db := testDB(t)
	store := NewClientStore(db)
	if err := store.Create(config.Client{Name: "no-quota", AccessKey: "k1"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	clients, err := store.ListWithQuota()
	if err != nil {
		t.Fatalf("ListWithQuota: %v", err)
	}
	if len(clients) != 0 {
		t.Errorf("all-zero client should not appear, got %d", len(clients))
	}
}

func TestClientStoreListWithQuotaMixed(t *testing.T) {
	db := testDB(t)
	store := NewClientStore(db)

	// 5 clients: 2 with quota, 3 without.
	cases := []config.Client{
		{Name: "a", AccessKey: "k1"},                                  // all zero
		{Name: "b", AccessKey: "k2"},                                  // all zero
		{Name: "c", AccessKey: "k3", QuotaDailyUSD: 10},               // has quota
		{Name: "d", AccessKey: "k4"},                                  // all zero
		{Name: "e", AccessKey: "k5", QuotaWeeklyUSD: 100, QuotaMonthlyUSD: 500}, // has quota
	}
	for _, c := range cases {
		if err := store.Create(c); err != nil {
			t.Fatalf("create %s: %v", c.Name, err)
		}
	}

	clients, err := store.ListWithQuota()
	if err != nil {
		t.Fatalf("ListWithQuota: %v", err)
	}
	if len(clients) != 2 {
		t.Fatalf("expected 2 clients, got %d: %+v", len(clients), clients)
	}

	names := []string{clients[0].Name, clients[1].Name}
	sort.Strings(names)
	if names[0] != "c" || names[1] != "e" {
		t.Errorf("expected [c, e], got %v", names)
	}
}

func TestClientStoreListWithQuotaAllThree(t *testing.T) {
	db := testDB(t)
	store := NewClientStore(db)

	c := config.Client{
		Name:            "all-three",
		AccessKey:       "k1",
		QuotaDailyUSD:   1,
		QuotaWeeklyUSD:  5,
		QuotaMonthlyUSD: 20,
	}
	if err := store.Create(c); err != nil {
		t.Fatalf("create: %v", err)
	}

	clients, err := store.ListWithQuota()
	if err != nil {
		t.Fatalf("ListWithQuota: %v", err)
	}
	if len(clients) != 1 {
		t.Fatalf("expected 1, got %d", len(clients))
	}
	got := clients[0]
	if got.QuotaDailyUSD != 1 || got.QuotaWeeklyUSD != 5 || got.QuotaMonthlyUSD != 20 {
		t.Errorf("unexpected quota fields: %+v", got)
	}
}

func TestClientStoreListWithQuotaTiny(t *testing.T) {
	db := testDB(t)
	store := NewClientStore(db)

	// 3 clients: zero / tiny-but-positive / normal.
	cases := []config.Client{
		{Name: "zero", AccessKey: "k1"},
		{Name: "tiny", AccessKey: "k2", QuotaDailyUSD: 0.01},
		{Name: "normal", AccessKey: "k3", QuotaWeeklyUSD: 10},
	}
	for _, c := range cases {
		if err := store.Create(c); err != nil {
			t.Fatalf("create %s: %v", c.Name, err)
		}
	}

	clients, err := store.ListWithQuota()
	if err != nil {
		t.Fatalf("ListWithQuota: %v", err)
	}
	if len(clients) != 2 {
		t.Fatalf("expected 2, got %d: %+v", len(clients), clients)
	}

	names := []string{clients[0].Name, clients[1].Name}
	sort.Strings(names)
	if names[0] != "normal" || names[1] != "tiny" {
		t.Errorf("expected [normal, tiny], got %v", names)
	}
}
