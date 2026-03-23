package main

import (
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ycgame/azure-proxy/internal/admin"
	"github.com/ycgame/azure-proxy/internal/auth"
	"github.com/ycgame/azure-proxy/internal/catalog"
	"github.com/ycgame/azure-proxy/internal/config"
	"github.com/ycgame/azure-proxy/internal/logging"
	appmiddleware "github.com/ycgame/azure-proxy/internal/middleware"
	"github.com/ycgame/azure-proxy/internal/nosql"
	"github.com/ycgame/azure-proxy/internal/proxy"
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
	clientStore := nosql.NewClientStore(cfg.DataFiles.ClientsFile)
	adminUsers := admin.NewUserStore(cfg.DataFiles.AdminUsersFile)
	auditStore := admin.NewAuditStore(cfg.DataFiles.AdminAuditFile)
	sessionManager := admin.NewSessionManager(cfg.AdminSession, appLogger)
	clients, err := clientStore.List()
	if err != nil {
		appLogger.Error("failed to load clients file",
			"path", cfg.DataFiles.ClientsFile,
			"error", err,
		)
		os.Exit(1)
	}

	appLogger.Info("configuration loaded",
		"config_path", *configPath,
		"config_bind", configBind,
		"effective_bind", bindAddr,
		"config_log_level", cfg.Logging.Level,
		"effective_log_level", logLevel,
		"azure_targets", len(cfg.AzureTargets),
		"clients", len(clients),
	)

	authStore := auth.NewStore()
	if err := authStore.LoadFromConfig(clients); err != nil {
		appLogger.Error("failed to initialize auth store", "error", err)
		os.Exit(1)
	}
	existingAdmins, err := adminUsers.List()
	if err != nil {
		appLogger.Error("failed to load admin users file", "path", cfg.DataFiles.AdminUsersFile, "error", err)
		os.Exit(1)
	}
	if len(existingAdmins) == 0 {
		seedErr := adminUsers.SeedDefaultUser(config.AdminUser{
			Username:     "admin",
			PasswordHash: admin.HashPassword("admin123", "default-salt"),
			Role:         "admin",
		})
		if seedErr != nil {
			appLogger.Warn("failed to seed default admin user", "error", seedErr)
		} else {
			appLogger.Info("seeded default admin user (admin / admin123) — change the password immediately")
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

	router := chi.NewRouter()
	router.Use(appmiddleware.RequestID())
	router.Use(appmiddleware.Recoverer(appLogger))
	router.Use(appmiddleware.AccessLogger(logManager.Access()))

	router.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	portal := admin.NewPortal(adminUsers, sessionManager, auditStore, appLogger)
	router.Get("/login", portal.HandleLoginPage)
	router.Post("/login", portal.HandleLogin)
	router.Post("/logout", portal.HandleLogout)

	adminRouter := chi.NewRouter()
	adminRouter.Use(sessionManager.Middleware)
	adminRouter.Mount("/", admin.NewHandler(manager, authStore, proxyService, auditStore, adminUsers, modelCatalog, appLogger))
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
		_, _ = w.Write([]byte(fmt.Sprintf(`{"message":"pong","client":"%s"}`, clientName)))
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
