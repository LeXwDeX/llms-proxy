package admin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ycgame/llms-proxy/internal/auth"
	"github.com/ycgame/llms-proxy/internal/catalog"
	"github.com/ycgame/llms-proxy/internal/config"
	"github.com/ycgame/llms-proxy/internal/nosql"
	"github.com/ycgame/llms-proxy/internal/proxy"
	"github.com/ycgame/llms-proxy/internal/usage"
)

// testStores creates a bbolt DB and all stores for testing.
type testStores struct {
	clientStore      *nosql.ClientStore
	modelCostStore   *nosql.ModelCostStore
	usageStore       *nosql.UsageStore
	userStore        *nosql.UserStore
	auditStore       *nosql.AuditStore
	targetStore      *nosql.TargetStore
	copilotPoolStore *nosql.CopilotPoolStore
}

func setupTestStores(t *testing.T, tempDir string) testStores {
	t.Helper()
	dbPath := filepath.Join(tempDir, "test.db")
	db, err := nosql.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return testStores{
		clientStore:      nosql.NewClientStore(db),
		modelCostStore:   nosql.NewModelCostStore(db),
		usageStore:       nosql.NewUsageStore(db),
		userStore:        nosql.NewUserStore(db),
		auditStore:       nosql.NewAuditStore(db),
		targetStore:      nosql.NewTargetStore(db),
		copilotPoolStore: nosql.NewCopilotPoolStore(db),
	}
}

func seedClients(t *testing.T, store *nosql.ClientStore, clients []config.Client) {
	t.Helper()
	for _, c := range clients {
		if err := store.Create(c); err != nil {
			t.Fatalf("failed to seed client %q: %v", c.Name, err)
		}
	}
}

func TestHandlerUIEntry(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")
	cfg := testConfig(tempDir, 1, []string{"k1"})
	clients := testClients([]string{"k1"})
	writeConfigFile(t, configPath, cfg)

	stores := setupTestStores(t, tempDir)
	seedClients(t, stores.clientStore, clients)

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	manager := config.NewManager(configPath)
	store := auth.NewStore()
	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatalf("failed to init auth store: %v", err)
	}
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("failed to init proxy service: %v", err)
	}

	h := NewHandler(manager, store, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, stores.copilotPoolStore, nil, nil, nil, logger)

	for _, route := range []string{"/", "/ui"} {
		t.Run(route, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com"+route, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("ui entry expected 200, got %d", rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
				t.Fatalf("expected text/html content-type, got %q", ct)
			}

			body := rec.Body.String()
			if !strings.Contains(body, "客户端管理") || !strings.Contains(body, "消费统计") {
				t.Fatalf("ui page missing expected tab labels")
			}
			if !strings.Contains(body, "/admin/data/clients") || !strings.Contains(body, "/admin/data/usage/aggregate") {
				t.Fatalf("ui page missing expected admin api endpoints")
			}
		})
	}
}

func TestHandlerUIEntryWhenMountedUnderAuth(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")
	cfg := testConfig(tempDir, 1, []string{"k1"})
	clients := testClients([]string{"k1"})
	writeConfigFile(t, configPath, cfg)

	stores := setupTestStores(t, tempDir)
	seedClients(t, stores.clientStore, clients)

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	manager := config.NewManager(configPath)
	store := auth.NewStore()
	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatalf("failed to init auth store: %v", err)
	}
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("failed to init proxy service: %v", err)
	}

	adminHandler := NewHandler(manager, store, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, stores.copilotPoolStore, nil, nil, nil, logger)
	protected := chi.NewRouter()
	protected.Use(auth.Middleware(store, logger))
	protected.Mount("/admin", adminHandler)

	rec := httptest.NewRecorder()
	protected.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com/admin/ui", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", rec.Code)
	}

	withAuth := httptest.NewRequest(http.MethodGet, "http://example.com/admin/ui", nil)
	withAuth.Header.Set("Authorization", "Bearer k1")
	rec = httptest.NewRecorder()
	protected.ServeHTTP(rec, withAuth)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with token, got %d", rec.Code)
	}
}

func TestHandlerHealthz(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")
	cfg := testConfig(tempDir, 2, []string{"k1"})
	clients := testClients([]string{"k1"})
	writeConfigFile(t, configPath, cfg)

	stores := setupTestStores(t, tempDir)
	seedClients(t, stores.clientStore, clients)

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	manager := config.NewManager(configPath)
	store := auth.NewStore()
	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatalf("failed to init auth store: %v", err)
	}
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("failed to init proxy service: %v", err)
	}

	h := NewHandler(manager, store, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, stores.copilotPoolStore, nil, nil, nil, logger)
	req := httptest.NewRequest(http.MethodGet, "http://example.com/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	var resp struct {
		Status      string               `json:"status"`
		Targets     []proxy.TargetStatus `json:"targets"`
		TargetCount int                  `json:"target_count"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Status == "" {
		t.Fatal("expected status field to be populated")
	}
	if resp.TargetCount != 2 {
		t.Fatalf("expected target_count 2, got %d", resp.TargetCount)
	}
	if len(resp.Targets) != 2 {
		t.Fatalf("expected 2 targets, got %d", len(resp.Targets))
	}
}

func TestHandlerMetrics(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")
	cfg := testConfig(tempDir, 1, []string{"k1"})
	clients := testClients([]string{"k1"})
	writeConfigFile(t, configPath, cfg)

	stores := setupTestStores(t, tempDir)
	seedClients(t, stores.clientStore, clients)

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	manager := config.NewManager(configPath)
	store := auth.NewStore()
	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatalf("failed to init auth store: %v", err)
	}
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("failed to init proxy service: %v", err)
	}

	h := NewHandler(manager, store, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, stores.copilotPoolStore, nil, nil, nil, logger)
	req := httptest.NewRequest(http.MethodGet, "http://example.com/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	var resp struct {
		UptimeSeconds float64 `json:"uptime_seconds"`
		Targets       int     `json:"targets"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.UptimeSeconds < 0 {
		t.Fatalf("expected non-negative uptime, got %f", resp.UptimeSeconds)
	}
	if resp.Targets != 1 {
		t.Fatalf("expected targets 1, got %d", resp.Targets)
	}
}

func TestHandlerReloadConfig(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")

	initial := testConfig(tempDir, 1, []string{"k1"})
	initialClients := testClients([]string{"k1"})
	writeConfigFile(t, configPath, initial)

	stores := setupTestStores(t, tempDir)
	seedClients(t, stores.clientStore, initialClients)

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	manager := config.NewManager(configPath)
	store := auth.NewStore()
	if err := store.LoadFromConfig(initialClients); err != nil {
		t.Fatalf("failed to init auth store: %v", err)
	}
	service, err := proxy.NewService(initial, logger)
	if err != nil {
		t.Fatalf("failed to init proxy service: %v", err)
	}

	h := NewHandler(manager, store, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, stores.copilotPoolStore, nil, nil, nil, logger)

	// Update config with 2 targets and new client.
	// Also update the bbolt client store to have k2 instead of k1.
	updated := testConfig(tempDir, 2, []string{"k2"})
	writeConfigFile(t, configPath, updated)
	// Delete old client, add new one in bbolt.
	_ = stores.clientStore.Delete("client-1")
	updatedClients := testClients([]string{"k2"})
	seedClients(t, stores.clientStore, updatedClients)

	req := httptest.NewRequest(http.MethodPost, "http://example.com/config/reload", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d (%s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		Status  string `json:"status"`
		Targets int    `json:"targets"`
		Clients int    `json:"clients"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Status != "ok" {
		t.Fatalf("expected status ok, got %q", resp.Status)
	}
	if resp.Targets != 2 || resp.Clients != 1 {
		t.Fatalf("unexpected counts: targets=%d clients=%d", resp.Targets, resp.Clients)
	}

	current, err := manager.Current()
	if err != nil {
		t.Fatalf("expected manager.Current to succeed, got %v", err)
	}
	if len(current.Targets) != 2 {
		t.Fatalf("expected manager cache to be updated to 2 targets, got %d", len(current.Targets))
	}

	if _, ok := store.Authenticate("k2"); !ok {
		t.Fatal("expected auth store to contain updated client key k2")
	}
	if _, ok := store.Authenticate("k1"); ok {
		t.Fatal("expected auth store to drop old client key k1")
	}
}

func TestHandlerReloadConfigRejectsInvalidProxyConfig(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")

	initial := testConfig(tempDir, 1, []string{"k1"})
	initialClients := testClients([]string{"k1"})
	writeConfigFile(t, configPath, initial)

	stores := setupTestStores(t, tempDir)
	seedClients(t, stores.clientStore, initialClients)

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	manager := config.NewManager(configPath)
	manager.Replace(initial)

	store := auth.NewStore()
	if err := store.LoadFromConfig(initialClients); err != nil {
		t.Fatalf("failed to init auth store: %v", err)
	}

	service, err := proxy.NewService(initial, logger)
	if err != nil {
		t.Fatalf("failed to init proxy service: %v", err)
	}

	h := NewHandler(manager, store, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, stores.copilotPoolStore, nil, nil, nil, logger)

	invalid := testConfig(tempDir, 1, []string{"k2"})
	invalid.Targets[0].Endpoint = "not-a-valid-url"
	writeConfigFile(t, configPath, invalid)

	req := httptest.NewRequest(http.MethodPost, "http://example.com/config/reload", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d (%s)", rec.Code, rec.Body.String())
	}

	if _, ok := store.Authenticate("k1"); !ok {
		t.Fatal("expected original key k1 to remain after rejected reload")
	}

	current, err := manager.Current()
	if err != nil {
		t.Fatalf("expected manager.Current to return cached config, got %v", err)
	}
	if len(current.Targets) != 1 || current.Targets[0].Endpoint != initial.Targets[0].Endpoint {
		t.Fatalf("expected manager cache to remain unchanged, got %#v", current.Targets)
	}
}

func TestHandlerDataClientsCRUD(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")

	cfg := testConfig(tempDir, 1, []string{"k1"})
	clients := testClients([]string{"k1"})
	writeConfigFile(t, configPath, cfg)

	stores := setupTestStores(t, tempDir)
	seedClients(t, stores.clientStore, clients)

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	manager := config.NewManager(configPath)
	store := auth.NewStore()
	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatalf("failed to init auth store: %v", err)
	}
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("failed to init proxy service: %v", err)
	}

	h := NewHandler(manager, store, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, stores.copilotPoolStore, nil, nil, nil, logger)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com/data/clients", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list clients expected 200, got %d", rec.Code)
	}

	body := bytes.NewBufferString(`{"name":"client-2","access_key":"k2","allowed_targets":[]}`)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://example.com/data/clients", body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create client expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := store.Authenticate("k2"); !ok {
		t.Fatalf("expected auth store updated with k2")
	}

	body = bytes.NewBufferString(`{"name":"client-2","access_key":"k3","allowed_targets":[]}`)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "http://example.com/data/clients/client-2", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("update client expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := store.Authenticate("k3"); !ok {
		t.Fatalf("expected auth store updated with k3")
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "http://example.com/data/clients/client-1", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete client expected 204, got %d body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := store.Authenticate("k1"); ok {
		t.Fatalf("expected client-1 removed from auth store")
	}
}

func TestHandlerUpdateTargetPreservesKeyPoolWithExistingRefs(t *testing.T) {
	tempDir := t.TempDir()
	initialKeys := []string{"sk-primary-real-0001", "sk-secondary-real-0002", "sk-secondary-real-0003"}
	h, _, stores := newTargetUpdateTestHandler(t, tempDir, initialKeys)

	body := map[string]any{
		"endpoint_type":            "azure_openai",
		"endpoint":                 "https://example.com",
		"resource_path_prefix":     "/openai",
		"api_key":                  "__existing_key_index__:0",
		"api_keys":                 []string{"__existing_key_index__:1", "__existing_key_index__:2"},
		"allow_bearer_passthrough": false,
		"model_mappings": []map[string]any{{"upstream": "gpt-4o"}, {"upstream": "gpt-4o-mini"}},
		"sse_auto_aggregate":       true,
	}
	rec := putTarget(t, h, "target-1", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("update target expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	updated := mustGetTarget(t, stores.targetStore, "target-1")
	if updated.APIKey != initialKeys[0] {
		t.Fatalf("APIKey overwritten: got %q want %q", updated.APIKey, initialKeys[0])
	}
	if got, want := updated.APIKeys, initialKeys[1:]; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("APIKeys changed: got %#v want %#v", got, want)
	}
	if got := modelMappingsUpstreams(updated.ModelMappings); got != "gpt-4o,gpt-4o-mini" {
		t.Fatalf("model_mappings upstream names not updated, got %q", got)
	}
}

// modelMappingsUpstreams joins the upstream names from ModelMappings into a
// comma-separated string, mirroring the old AllowedModels join format for test
// assertions.
func modelMappingsUpstreams(mappings []config.ModelMapping) string {
	names := make([]string, 0, len(mappings))
	for _, m := range mappings {
		names = append(names, m.Upstream)
	}
	return strings.Join(names, ",")
}

func TestHandlerListTargetsReturnsPaused(t *testing.T) {
	tempDir := t.TempDir()
	h, _, _ := newTargetUpdateTestHandler(t, tempDir, []string{"sk-primary-real-0001"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com/data/targets", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list targets expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Targets []struct {
			Name   string `json:"name"`
			Paused bool   `json:"paused"`
		} `json:"targets"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Targets) != 1 || resp.Targets[0].Name != "target-1" {
		t.Fatalf("unexpected targets: %#v", resp.Targets)
	}
}

func TestHandlerListAndUpdateTargetPreservesImageOperation(t *testing.T) {
	tempDir := t.TempDir()
	h, _, stores := newTargetUpdateTestHandler(t, tempDir, []string{"sk-primary-real-0001"})
	initial := config.Target{
		Name:           "target-1",
		EndpointType:   config.EndpointTypeOpenAIImage,
		Endpoint:       "https://image.example.com/v1/images/edits",
		APIKey:         "sk-primary-real-0001",
		ImageOperation: config.ImageOperationEdits,
		ModelMappings: []config.ModelMapping{{Upstream: "gpt-image-2"}},
	}
	if err := stores.targetStore.Create(initial); err != nil {
		t.Fatalf("seed image operation target: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com/data/targets", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list targets expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Targets []struct {
			Name           string `json:"name"`
			EndpointType   string `json:"endpoint_type"`
			ImageOperation string `json:"image_operation"`
		} `json:"targets"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Targets) != 1 || resp.Targets[0].EndpointType != config.EndpointTypeOpenAIImage || resp.Targets[0].ImageOperation != config.ImageOperationEdits {
		t.Fatalf("list did not include image_operation: %#v", resp.Targets)
	}

	body := map[string]any{
		"endpoint_type":            config.EndpointTypeOpenAIImage,
		"endpoint":                 "https://image.example.com/v1/images/edits",
		"image_operation":          config.ImageOperationEdits,
		"api_key":                  "__existing_key_index__:0",
		"allow_bearer_passthrough": false,
		"model_mappings": []map[string]any{{"upstream": "gpt-image-2"}},
		"sse_auto_aggregate":       true,
	}
	rec = putTarget(t, h, "target-1", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("update target expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	updated := mustGetTarget(t, stores.targetStore, "target-1")
	if updated.ImageOperation != config.ImageOperationEdits {
		t.Fatalf("image_operation not preserved after update: %+v", updated)
	}
}

func TestHandlerUpdateTargetSetsPausedInStoreAndRuntime(t *testing.T) {
	tempDir := t.TempDir()
	initialKeys := []string{"sk-primary-real-0001", "sk-secondary-real-0002"}
	h, _, stores, service := newTargetUpdateTestHandlerWithService(t, tempDir, initialKeys)

	body := targetUpdateBody("__existing_key_index__:0", []string{"__existing_key_index__:1"})
	body["paused"] = true
	rec := putTarget(t, h, "target-1", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("update target expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	updated := mustGetTarget(t, stores.targetStore, "target-1")
	if !updated.Paused {
		t.Fatalf("expected paused=true in target store")
	}
	statuses := service.TargetStatuses(time.Now())
	if len(statuses) != 1 || !statuses[0].Paused {
		t.Fatalf("expected runtime paused=true, got %#v", statuses)
	}

	body["paused"] = false
	rec = putTarget(t, h, "target-1", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("update target expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	updated = mustGetTarget(t, stores.targetStore, "target-1")
	if updated.Paused {
		t.Fatalf("expected paused=false in target store")
	}
	statuses = service.TargetStatuses(time.Now())
	if len(statuses) != 1 || statuses[0].Paused {
		t.Fatalf("expected runtime paused=false, got %#v", statuses)
	}
}

func TestHandlerPauseResumeTargetPreservesAPIKeys(t *testing.T) {
	tempDir := t.TempDir()
	initialKeys := []string{"sk-primary-real-0001", "sk-secondary-real-0002", "sk-secondary-real-0003"}
	h, _, stores, service := newTargetUpdateTestHandlerWithService(t, tempDir, initialKeys)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "http://example.com/data/targets/target-1/pause", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("pause target expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	paused := mustGetTarget(t, stores.targetStore, "target-1")
	if !paused.Paused {
		t.Fatalf("expected paused=true")
	}
	if paused.APIKey != initialKeys[0] || fmt.Sprint(paused.APIKeys) != fmt.Sprint(initialKeys[1:]) {
		t.Fatalf("keys changed on pause: %#v", paused)
	}
	statuses := service.TargetStatuses(time.Now())
	if len(statuses) != 1 || !statuses[0].Paused {
		t.Fatalf("expected runtime paused=true, got %#v", statuses)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "http://example.com/data/targets/target-1/resume", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("resume target expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	resumed := mustGetTarget(t, stores.targetStore, "target-1")
	if resumed.Paused {
		t.Fatalf("expected paused=false")
	}
	if resumed.APIKey != initialKeys[0] || fmt.Sprint(resumed.APIKeys) != fmt.Sprint(initialKeys[1:]) {
		t.Fatalf("keys changed on resume: %#v", resumed)
	}
	statuses = service.TargetStatuses(time.Now())
	if len(statuses) != 1 || statuses[0].Paused {
		t.Fatalf("expected runtime paused=false, got %#v", statuses)
	}
}

func TestHandlerPauseResumeURLUnescapesChineseTargetName(t *testing.T) {
	tempDir := t.TempDir()
	name := "网宿-Gemini"
	initialKeys := []string{"sk-primary-real-0001", "sk-secondary-real-0002"}
	h, stores, service := newNamedTargetUpdateTestHandler(t, tempDir, name, initialKeys)
	escapedName := url.PathEscape(name)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "http://example.com/data/targets/"+escapedName+"/pause", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("pause Chinese target expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	paused := mustGetTarget(t, stores.targetStore, name)
	if !paused.Paused {
		t.Fatalf("expected paused=true for %q", name)
	}
	statuses := service.TargetStatuses(time.Now())
	if len(statuses) != 1 || statuses[0].Name != name || !statuses[0].Paused {
		t.Fatalf("expected runtime paused=true for %q, got %#v", name, statuses)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "http://example.com/data/targets/"+escapedName+"/resume", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("resume Chinese target expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	resumed := mustGetTarget(t, stores.targetStore, name)
	if resumed.Paused {
		t.Fatalf("expected paused=false for %q", name)
	}
	statuses = service.TargetStatuses(time.Now())
	if len(statuses) != 1 || statuses[0].Name != name || statuses[0].Paused {
		t.Fatalf("expected runtime paused=false for %q, got %#v", name, statuses)
	}
}

func TestHandlerUpdateTargetAcceptsExactMaskedExistingKeys(t *testing.T) {
	tempDir := t.TempDir()
	initialKeys := []string{"sk-primary-real-0001", "sk-secondary-real-0002", "sk-secondary-real-0003"}
	h, _, stores := newTargetUpdateTestHandler(t, tempDir, initialKeys)

	body := targetUpdateBody(initialKeys[0], []string{maskKey(initialKeys[1]), maskKey(initialKeys[2])})
	rec := putTarget(t, h, "target-1", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("update target expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	updated := mustGetTarget(t, stores.targetStore, "target-1")
	if updated.APIKey != initialKeys[0] {
		t.Fatalf("APIKey overwritten: got %q want %q", updated.APIKey, initialKeys[0])
	}
	if got, want := updated.APIKeys, initialKeys[1:]; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("APIKeys changed: got %#v want %#v", got, want)
	}
}

func TestHandlerUpdateTargetDeletesOmittedSecondaryKey(t *testing.T) {
	tempDir := t.TempDir()
	initialKeys := []string{"sk-primary-real-0001", "sk-secondary-real-0002", "sk-secondary-real-0003"}
	h, _, stores := newTargetUpdateTestHandler(t, tempDir, initialKeys)

	body := targetUpdateBody("__existing_key_index__:0", []string{"__existing_key_index__:2"})
	rec := putTarget(t, h, "target-1", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("update target expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	updated := mustGetTarget(t, stores.targetStore, "target-1")
	if got, want := updated.APIKeys, []string{initialKeys[2]}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("APIKeys after delete got %#v want %#v", got, want)
	}
}

func TestHandlerUpdateTargetKeepsExistingSecondaryAndAddsNewOne(t *testing.T) {
	tempDir := t.TempDir()
	initialKeys := []string{"sk-primary-real-0001", "sk-secondary-real-0002", "sk-secondary-real-0003"}
	h, _, stores := newTargetUpdateTestHandler(t, tempDir, initialKeys)

	body := targetUpdateBody("__existing_key_index__:0", []string{"__existing_key_index__:1", "sk-secondary-real-0004"})
	rec := putTarget(t, h, "target-1", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("update target expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	updated := mustGetTarget(t, stores.targetStore, "target-1")
	if got, want := updated.APIKeys, []string{initialKeys[1], "sk-secondary-real-0004"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("APIKeys after add got %#v want %#v", got, want)
	}
}

func TestHandlerUpdateTargetRejectsUnrecognizedMaskedLookingKey(t *testing.T) {
	tempDir := t.TempDir()
	initialKeys := []string{"sk-primary-real-0001", "sk-secondary-real-0002"}
	h, _, stores := newTargetUpdateTestHandler(t, tempDir, initialKeys)

	body := targetUpdateBody("__existing_key_index__:0", []string{"sk-b...dead"})
	rec := putTarget(t, h, "target-1", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("update target expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}

	updated := mustGetTarget(t, stores.targetStore, "target-1")
	if got, want := updated.APIKeys, initialKeys[1:]; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("APIKeys should remain unchanged after reject, got %#v want %#v", got, want)
	}
}

func TestHandlerUpdateTargetRejectsOutOfRangeExistingRef(t *testing.T) {
	tempDir := t.TempDir()
	initialKeys := []string{"sk-primary-real-0001", "sk-secondary-real-0002"}
	h, _, stores := newTargetUpdateTestHandler(t, tempDir, initialKeys)

	body := targetUpdateBody("__existing_key_index__:0", []string{"__existing_key_index__:9"})
	rec := putTarget(t, h, "target-1", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("update target expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}

	updated := mustGetTarget(t, stores.targetStore, "target-1")
	if got, want := updated.APIKeys, initialKeys[1:]; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("APIKeys should remain unchanged after reject, got %#v want %#v", got, want)
	}
}

func newTargetUpdateTestHandler(t *testing.T, tempDir string, keys []string) (http.Handler, *config.Manager, testStores) {
	h, manager, stores, _ := newTargetUpdateTestHandlerWithService(t, tempDir, keys)
	return h, manager, stores
}

func newTargetUpdateTestHandlerWithService(t *testing.T, tempDir string, keys []string) (http.Handler, *config.Manager, testStores, *proxy.Service) {
	t.Helper()
	configPath := filepath.Join(tempDir, "config.json")
	cfg := testConfig(tempDir, 1, []string{"k1"})
	cfg.Targets[0].APIKey = keys[0]
	if len(keys) > 1 {
		cfg.Targets[0].APIKeys = append([]string(nil), keys[1:]...)
	}
	clients := testClients([]string{"k1"})
	writeConfigFile(t, configPath, cfg)

	stores := setupTestStores(t, tempDir)
	seedClients(t, stores.clientStore, clients)
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	manager := config.NewManager(configPath)
	store := auth.NewStore()
	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatalf("failed to init auth store: %v", err)
	}
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("failed to init proxy service: %v", err)
	}
	h := NewHandler(manager, store, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, stores.copilotPoolStore, nil, nil, nil, logger)
	return h, manager, stores, service
}

func newNamedTargetUpdateTestHandler(t *testing.T, tempDir, name string, keys []string) (http.Handler, testStores, *proxy.Service) {
	t.Helper()
	configPath := filepath.Join(tempDir, "config.json")
	cfg := testConfig(tempDir, 1, []string{"k1"})
	cfg.Targets[0].Name = name
	cfg.Targets[0].APIKey = keys[0]
	if len(keys) > 1 {
		cfg.Targets[0].APIKeys = append([]string(nil), keys[1:]...)
	}
	clients := testClients([]string{"k1"})
	writeConfigFile(t, configPath, cfg)

	stores := setupTestStores(t, tempDir)
	seedClients(t, stores.clientStore, clients)
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	manager := config.NewManager(configPath)
	store := auth.NewStore()
	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatalf("failed to init auth store: %v", err)
	}
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("failed to init proxy service: %v", err)
	}
	h := NewHandler(manager, store, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, stores.copilotPoolStore, nil, nil, nil, logger)
	return h, stores, service
}

func targetUpdateBody(apiKey string, apiKeys []string) map[string]any {
	return map[string]any{
		"endpoint_type":            "azure_openai",
		"endpoint":                 "https://example.com",
		"resource_path_prefix":     "/openai",
		"api_key":                  apiKey,
		"api_keys":                 apiKeys,
		"allow_bearer_passthrough": false,
		"model_mappings": []map[string]any{{"upstream": "gpt-4o"}},
		"sse_auto_aggregate":       true,
	}
}

func putTarget(t *testing.T, h http.Handler, name string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal update body: %v", err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "http://example.com/data/targets/"+name, bytes.NewReader(payload)))
	return rec
}

func mustGetTarget(t *testing.T, store *nosql.TargetStore, name string) config.Target {
	t.Helper()
	targets, err := store.List()
	if err != nil {
		t.Fatalf("list targets: %v", err)
	}
	for _, target := range targets {
		if target.Name == name {
			return target
		}
	}
	t.Fatalf("target %q not found", name)
	return config.Target{}
}

func TestHandlerUsageSummary(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")

	cfg := testConfig(tempDir, 1, []string{"k1"})
	clients := testClients([]string{"k1"})
	writeConfigFile(t, configPath, cfg)

	stores := setupTestStores(t, tempDir)
	seedClients(t, stores.clientStore, clients)

	// Record a usage event via bbolt store.
	now := time.Now().UTC()
	if err := stores.usageStore.Record(usage.Event{Timestamp: now.Add(-10 * time.Minute), ClientName: "client-1", Model: "gpt-4o", InputTokens: 100, OutputTokens: 50}); err != nil {
		t.Fatalf("record usage event: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	manager := config.NewManager(configPath)
	store := auth.NewStore()
	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatalf("failed to init auth store: %v", err)
	}
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("failed to init proxy service: %v", err)
	}

	h := NewHandler(manager, store, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, stores.copilotPoolStore, nil, nil, nil, logger)

	body := bytes.NewBufferString(`{"input_per_1m_tokens":10,"output_per_1m_tokens":20,"cached_input_per_1m_tokens":0}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "http://example.com/data/model-costs/gpt-4o", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("upsert model cost expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com/data/usage/summary", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("summary expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	var payload struct {
		LastHour struct {
			Requests int64 `json:"requests"`
		} `json:"last_hour"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatalf("decode summary response: %v", err)
	}
	if payload.LastHour.Requests == 0 {
		t.Fatalf("expected summary last_hour requests > 0")
	}
}

func writeConfigFile(t *testing.T, path string, cfg *config.Config) {
	t.Helper()
	payload, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("failed to marshal config: %v", err)
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}
}

func testConfig(tempDir string, targetCount int, clientKeys []string) *config.Config {
	_ = clientKeys
	targets := make([]config.Target, 0, targetCount)
	for i := 0; i < targetCount; i++ {
		targets = append(targets, config.Target{
			Name:               fmt.Sprintf("target-%d", i+1),
			Endpoint:           "https://example.com",
			ResourcePathPrefix: "/openai",
			APIKey:             "key",
			ModelMappings: []config.ModelMapping{{Upstream: "gpt-4o"}},
		})
	}

	return &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: targets,
		DataStore: config.DataStore{
			DBPath: filepath.Join(tempDir, "test.db"),
		},
		AdminSession: config.AdminSessionConfig{
			CookieName: "admin_sid",
			Secret:     "test-secret",
			TTLSeconds: 3600,
		},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: filepath.Join(tempDir, "logs", "access.log"),
			ErrorLog:  filepath.Join(tempDir, "logs", "error.log"),
		},
	}
}

func testClients(clientKeys []string) []config.Client {
	clients := make([]config.Client, 0, len(clientKeys))
	for i, key := range clientKeys {
		clients = append(clients, config.Client{
			Name:      fmt.Sprintf("client-%d", i+1),
			AccessKey: key,
		})
	}
	return clients
}

func TestToUsageCostTableCatalogFallback(t *testing.T) {
	// Build a small catalog for testing.
	cat, err := catalog.New()
	if err != nil {
		t.Fatalf("catalog.New() error: %v", err)
	}

	// Scenario 1: catalog only (no custom costs) — should use catalog default_cost
	table := toUsageCostTable(nil, cat)
	rate, ok := table.LookupCost("openai", "gpt-4o")
	if !ok {
		t.Fatal("expected catalog default cost for openai:gpt-4o, got not found")
	}
	if rate.InputPer1MTokens <= 0 || rate.OutputPer1MTokens <= 0 {
		t.Fatalf("expected positive catalog default rates, got %+v", rate)
	}
	catalogInput := rate.InputPer1MTokens

	// Scenario 2: custom override — should override catalog defaults
	customCosts := []nosql.ModelCost{
		{EndpointType: "openai", Model: "gpt-4o", InputPer1MTokens: 999, OutputPer1MTokens: 888},
	}
	table = toUsageCostTable(customCosts, cat)
	rate, ok = table.LookupCost("openai", "gpt-4o")
	if !ok {
		t.Fatal("expected cost for openai:gpt-4o after custom override")
	}
	if rate.InputPer1MTokens != 999 || rate.OutputPer1MTokens != 888 {
		t.Fatalf("expected custom rates (999, 888), got %+v", rate)
	}

	// Scenario 3: catalog without custom — other models still use catalog defaults
	rate2, ok := table.LookupCost("openai", "gpt-4o-mini")
	if !ok {
		t.Fatal("expected catalog default cost for openai:gpt-4o-mini (not overridden)")
	}
	if rate2.InputPer1MTokens <= 0 {
		t.Fatalf("expected positive catalog default for gpt-4o-mini, got %+v", rate2)
	}

	// Scenario 4: nil catalog — only custom costs
	table = toUsageCostTable(customCosts, nil)
	rate, ok = table.LookupCost("openai", "gpt-4o")
	if !ok || rate.InputPer1MTokens != 999 {
		t.Fatalf("expected custom rate with nil catalog, got ok=%v rate=%+v", ok, rate)
	}
	_, ok = table.LookupCost("openai", "gpt-4o-mini")
	if ok {
		t.Fatal("expected no cost for gpt-4o-mini with nil catalog and no custom entry")
	}

	// Confirm catalog default is different from custom value
	if catalogInput == 999 {
		t.Fatal("test setup issue: catalog default same as custom override value")
	}
}

func TestHandlerEndpointTypesEndpoint(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")
	cfg := testConfig(tempDir, 1, []string{"k1"})
	clients := testClients([]string{"k1"})
	writeConfigFile(t, configPath, cfg)

	stores := setupTestStores(t, tempDir)
	seedClients(t, stores.clientStore, clients)

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	manager := config.NewManager(configPath)
	store := auth.NewStore()
	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatalf("init auth store: %v", err)
	}
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("init proxy service: %v", err)
	}
	h := NewHandler(manager, store, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, stores.copilotPoolStore, nil, nil, nil, logger)

	req := httptest.NewRequest(http.MethodGet, "http://example.com/data/endpoint-types", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Count         int                       `json:"count"`
		EndpointTypes []config.EndpointTypeMeta `json:"endpoint_types"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Count == 0 || len(resp.EndpointTypes) == 0 {
		t.Fatal("expected non-empty endpoint types")
	}
	if resp.Count != len(resp.EndpointTypes) {
		t.Fatalf("count(%d) != len(%d)", resp.Count, len(resp.EndpointTypes))
	}
	// 必须包含 openai_image 类型
	want := map[string]bool{"openai_image": false}
	for _, m := range resp.EndpointTypes {
		if _, ok := want[m.Code]; ok {
			want[m.Code] = true
			if m.DisplayName == "" || m.ShortLabel == "" {
				t.Errorf("%s 缺少 DisplayName 或 ShortLabel", m.Code)
			}
		}
	}
	for code, found := range want {
		if !found {
			t.Errorf("API 未返回 %s", code)
		}
	}
}
