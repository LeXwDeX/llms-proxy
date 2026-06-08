package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ycgame/llms-proxy/internal/admin"
	"github.com/ycgame/llms-proxy/internal/auth"
	"github.com/ycgame/llms-proxy/internal/catalog"
	"github.com/ycgame/llms-proxy/internal/config"
	"github.com/ycgame/llms-proxy/internal/copilot"
	"github.com/ycgame/llms-proxy/internal/errorlog"
	"github.com/ycgame/llms-proxy/internal/logging"
	appmiddleware "github.com/ycgame/llms-proxy/internal/middleware"
	"github.com/ycgame/llms-proxy/internal/nosql"
	"github.com/ycgame/llms-proxy/internal/proxy"
	"github.com/ycgame/llms-proxy/internal/quota"
)

func main() {
	configPath := flag.String("config", "config/config.json", "path to the configuration file")
	flag.Parse()

	manager := config.NewManager(*configPath)
	cfg, created, err := manager.LoadOrInit()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}
	if created {
		fmt.Fprintf(os.Stderr, "default config generated at %s — add targets via admin UI\n", *configPath)
	}

	logLevel := cfg.Logging.Level
	if envLogLevel := strings.TrimSpace(os.Getenv("LOG_LEVEL")); envLogLevel != "" {
		logLevel = envLogLevel
	}

	logManager, err := logging.Setup(logging.Config{
		Level:         logLevel,
		AccessLogPath: cfg.Logging.AccessLog,
		ErrorLogPath:  cfg.Logging.ErrorLog,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize logging: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		if err := logManager.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to close log manager: %v\n", err)
		}
	}()

	configBind := strings.TrimSpace(cfg.Server.Bind)
	if configBind == "" {
		configBind = "0.0.0.0:8000"
	}
	bindAddr := configBind
	if envBind := strings.TrimSpace(os.Getenv("SERVER_BIND")); envBind != "" {
		bindAddr = envBind
	}

	appLogger := logManager.App()

	// 上游错误日志（旁路 access/error log，仅记 4xx/5xx/net error/panic）。
	// 路径可通过 UPSTREAM_ERROR_LOG_PATH 覆盖，默认 /var/log/llms-proxy/upstream-error.log。
	// 打不开时 Init 内部 slog.Warn 后降级为 noop，不阻断启动。
	errorlog.SetSlogger(appLogger)
	errorlog.Init(strings.TrimSpace(os.Getenv("UPSTREAM_ERROR_LOG_PATH")))
	defer func() {
		if err := errorlog.Close(); err != nil {
			appLogger.Warn("upstream-error log close failed", "error", err)
		}
	}()

	// Open bbolt database.
	dbPath := cfg.DataStore.DBPath
	db, err := nosql.OpenDB(dbPath)
	if err != nil {
		appLogger.Error("failed to open database", "path", dbPath, "error", err)
		os.Exit(1)
	}
	defer db.Close()

	// Migrate from legacy JSON files if needed.
	if err := nosql.MigrateFromJSON(db, cfg.DataFiles); err != nil {
		appLogger.Warn("json migration encountered errors", "error", err)
	}

	if migrated, err := nosql.MigrateTargetsFromConfig(db, cfg.Targets); err != nil {
		appLogger.Error("failed to migrate targets into database", "error", err)
		os.Exit(1)
	} else if migrated {
		if err := config.RemoveTargetsFromFile(*configPath); err != nil {
			appLogger.Warn("targets migrated but failed to remove them from config file", "error", err)
		} else {
			appLogger.Info("migrated targets into database and removed legacy config targets")
		}
	}

	// One-shot backfill of hourly usage aggregation (idempotent via meta marker).
	// Must run BEFORE proxy starts accepting traffic so reads see consistent agg data.
	backfillStart := time.Now()
	if err := nosql.BackfillUsageAgg(db); err != nil {
		appLogger.Error("usage agg backfill failed", "error", err)
		os.Exit(1)
	}
	appLogger.Info("usage agg backfill done", "duration", time.Since(backfillStart).String())

	// Create all bbolt-backed stores.
	clientStore := nosql.NewClientStore(db)
	targetStore := nosql.NewTargetStore(db)
	modelCostStore := nosql.NewModelCostStore(db)
	usageStore := nosql.NewUsageStore(db)
	userStore := nosql.NewUserStore(db)
	auditStore := nosql.NewAuditStore(db)
	copilotPoolStore := nosql.NewCopilotPoolStore(db)

	sessionManager := admin.NewSessionManager(cfg.AdminSession, appLogger)

	clients, err := clientStore.List()
	if err != nil {
		appLogger.Error("failed to load clients from database",
			"error", err,
		)
		os.Exit(1)
	}
	targets, err := targetStore.List()
	if err != nil {
		appLogger.Error("failed to load targets from database", "error", err)
		os.Exit(1)
	}
	cfg.Targets = targets
	manager.Replace(cfg)

	appLogger.Info("configuration loaded",
		"config_path", *configPath,
		"config_bind", configBind,
		"effective_bind", bindAddr,
		"config_log_level", cfg.Logging.Level,
		"effective_log_level", logLevel,
		"targets", len(cfg.Targets),
		"clients", len(clients),
		"db_path", dbPath,
	)

	// Trace store: 仅通过环境变量控制，不从 config.json 读取
	if envTrace := strings.TrimSpace(os.Getenv("TRACE_STORE_ENABLED")); envTrace == "true" {
		cfg.TraceStore.Enabled = true
		// 读取可选的磁盘配置
		if envMaxSize := strings.TrimSpace(os.Getenv("TRACE_STORE_MAX_SIZE_MB")); envMaxSize != "" {
			if v, err := strconv.Atoi(envMaxSize); err == nil && v > 0 {
				cfg.TraceStore.DiskMaxSizeMB = v
			} else {
				appLogger.Warn("invalid TRACE_STORE_MAX_SIZE_MB, using default", "value", envMaxSize)
			}
		}
		if envMaxBackups := strings.TrimSpace(os.Getenv("TRACE_STORE_MAX_BACKUPS")); envMaxBackups != "" {
			if v, err := strconv.Atoi(envMaxBackups); err == nil && v > 0 {
				cfg.TraceStore.DiskMaxBackups = v
			} else {
				appLogger.Warn("invalid TRACE_STORE_MAX_BACKUPS, using default", "value", envMaxBackups)
			}
		}
		if envTTL := strings.TrimSpace(os.Getenv("TRACE_STORE_TTL_HOURS")); envTTL != "" {
			if v, err := strconv.Atoi(envTTL); err == nil && v > 0 {
				cfg.TraceStore.DiskTTLHours = v
			} else {
				appLogger.Warn("invalid TRACE_STORE_TTL_HOURS, using default", "value", envTTL)
			}
		}
		appLogger.Info("trace store enabled via TRACE_STORE_ENABLED=true",
			"disk_max_size_mb", cfg.TraceStore.DiskMaxSizeMB,
			"disk_max_backups", cfg.TraceStore.DiskMaxBackups,
			"disk_ttl_hours", cfg.TraceStore.DiskTTLHours,
		)
	} else {
		cfg.TraceStore.Enabled = false
	}

	authStore := auth.NewStore()
	if err := authStore.LoadFromConfig(clients); err != nil {
		appLogger.Error("failed to initialize auth store", "error", err)
		os.Exit(1)
	}

	// Seed default admin user if the user store is empty.
	existingAdmins, err := userStore.List()
	if err != nil {
		appLogger.Error("failed to load admin users", "error", err)
		os.Exit(1)
	}
	if len(existingAdmins) == 0 {
		// Determine initial password: prefer ADMIN_PASSWORD env var, fallback to random.
		initialPassword := strings.TrimSpace(os.Getenv("ADMIN_PASSWORD"))
		passwordSource := "environment variable ADMIN_PASSWORD"
		if initialPassword == "" {
			randomBytes := make([]byte, 12)
			if _, err := rand.Read(randomBytes); err != nil {
				appLogger.Error("failed to generate random admin password", "error", err)
				os.Exit(1)
			}
			initialPassword = hex.EncodeToString(randomBytes)
			passwordSource = "random generation"
		}
		passwordHash, err := admin.HashPasswordWithRandomSalt(initialPassword)
		if err != nil {
			appLogger.Error("failed to hash admin password", "error", err)
			os.Exit(1)
		}
		seedErr := userStore.SeedDefaultUser(config.AdminUser{
			Username:     "admin",
			PasswordHash: passwordHash,
			Role:         "admin",
		})
		if seedErr != nil {
			appLogger.Warn("failed to seed default admin user", "error", seedErr)
		} else {
			appLogger.Info("seeded default admin user",
				"username", "admin",
				"password_source", passwordSource,
			)
			if passwordSource != "environment variable ADMIN_PASSWORD" {
				// Only print password to stderr when randomly generated (one-time visibility).
				fmt.Fprintf(os.Stderr, "\n"+
					"============================================================\n"+
					"  INITIAL ADMIN PASSWORD (shown only once)\n"+
					"  Username: admin\n"+
					"  Password: %s\n"+
					"  Or set ADMIN_PASSWORD env var before first start.\n"+
					"============================================================\n\n",
					initialPassword)
			}
		}
	}

	modelCatalog, err := catalog.New()
	if err != nil {
		appLogger.Warn("failed to load model catalog, continuing without defaults", "error", err)
	}

	proxyService, err := proxy.NewService(cfg, appLogger)
	if err != nil {
		appLogger.Error("failed to initialize proxy service", "error", err)
		os.Exit(1)
	}

	// Set usage recorder from the bbolt-backed store.
	proxyService.SetUsageRecorder(usageStore)

	// Create Copilot account store and services.
	copilotAcctStore := nosql.NewCopilotAccountStore(db, copilotPoolStore)
	copilotService := copilot.NewCopilotService(copilotAcctStore, copilotPoolStore, &http.Client{Timeout: 30 * time.Second}, appLogger)
	copilotQuotaMgr := copilot.NewQuotaManager(&http.Client{Timeout: 10 * time.Second}, "", appLogger)

	// Inject copilot service into proxy.
	proxyService.SetCopilotService(copilotService)

	// Initialize client quota manager（docs/quota-design.md §3）。
	// 周期性评估所有配置了 quota 的 client；超限时在请求准入阶段返回 429，并在 SSE 流期间触发 TCP RST 中断。
	// 构造失败时降级为 nil Manager，s.quotaManager == nil 跳过检查，不影响主链路。
	quotaMgr, quotaMgrErr := quota.New(quota.Options{
		Catalog:     modelCatalog,
		CostStore:   modelCostStore,
		UsageStore:  usageStore,
		ClientStore: clientStore,
		Logger:      appLogger,
	})
	if quotaMgrErr != nil {
		appLogger.Warn("failed to initialize quota manager, quota checks disabled", "error", quotaMgrErr)
		quotaMgr = nil
	}
	if quotaMgr != nil {
		quotaCtx, quotaCancel := context.WithCancel(context.Background())
		defer quotaCancel()
		if err := quotaMgr.Start(quotaCtx); err != nil {
			appLogger.Warn("quota manager start failed, quota checks disabled", "error", err)
		} else {
			proxyService.SetQuotaManager(quotaMgr)
			defer quotaMgr.Stop()
		}
	}

	// Start periodic quota sync.
	quotaSyncCtx, quotaSyncCancel := context.WithCancel(context.Background())
	defer quotaSyncCancel()
	copilotQuotaMgr.StartPeriodicSync(quotaSyncCtx, copilotAcctStore, copilot.DefaultQuotaSyncInterval)

	router := chi.NewRouter()
	router.Use(appmiddleware.RequestID())
	router.Use(appmiddleware.Recoverer(appLogger))
	router.Use(appmiddleware.AccessLogger(logManager.Access()))

	router.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	portal := admin.NewPortal(userStore, sessionManager, auditStore, appLogger)
	router.Get("/login", portal.HandleLoginPage)
	router.Post("/login", portal.HandleLogin)
	router.Post("/logout", portal.HandleLogout)

	adminRouter := chi.NewRouter()
	adminRouter.Use(sessionManager.Middleware)
	adminRouter.Mount("/", admin.NewHandler(manager, authStore, proxyService, auditStore, userStore, clientStore, targetStore, modelCostStore, usageStore, modelCatalog, copilotPoolStore, copilotService, copilotAcctStore, copilotQuotaMgr, quotaMgr, appLogger))
	router.Mount("/admin", adminRouter)

	protected := chi.NewRouter()
	protected.Use(auth.Middleware(authStore, appLogger))
	protected.Get("/api/ping", func(w http.ResponseWriter, r *http.Request) {
		principal, ok := auth.PrincipalFromContext(r.Context())
		clientName := ""
		if ok && principal != nil {
			clientName = principal.Name
		}
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]string{"message": "pong", "client": clientName}
		_ = json.NewEncoder(w).Encode(resp)
	})

	// Copilot passthrough routes — /copilot/auth, /copilot/quota, /copilot/models, /copilot/*
	protected.Route("/copilot", func(r chi.Router) {
		r.Get("/auth", proxyService.HandleCopilotAuth)            // GET /copilot/auth
		r.Get("/quota", proxyService.HandleCopilotQuotaSummary)   // GET /copilot/quota
		r.Get("/models", proxyService.HandleCopilotModels)        // GET /copilot/models
		r.HandleFunc("/*", proxyService.HandleCopilotPassthrough) // /copilot/* catch-all
	})

	protected.NotFound(proxyService.ServeHTTP)

	router.Mount("/", protected)

	server := &http.Server{
		Addr:              bindAddr,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       time.Duration(cfg.Server.RequestTimeoutSeconds) * time.Second,
		WriteTimeout:      time.Duration(cfg.Server.RequestTimeoutSeconds) * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	appLogger.Info("http server starting", "addr", bindAddr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		appLogger.Error("http server stopped unexpectedly", "error", err)
		os.Exit(1)
	}
}
