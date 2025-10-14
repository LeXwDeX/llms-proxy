package main

import (
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ycgame/azure-proxy/internal/admin"
	"github.com/ycgame/azure-proxy/internal/auth"
	"github.com/ycgame/azure-proxy/internal/config"
	"github.com/ycgame/azure-proxy/internal/logging"
	appmiddleware "github.com/ycgame/azure-proxy/internal/middleware"
	"github.com/ycgame/azure-proxy/internal/proxy"
)

func main() {
	configPath := flag.String("config", "config/config.json", "path to the configuration file")
	flag.Parse()

	manager := config.NewManager(*configPath)
	cfg, err := manager.Reload()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	logManager, err := logging.Setup(logging.Config{
		Level:         cfg.Logging.Level,
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

	appLogger := logManager.App()
	appLogger.Info("configuration loaded",
		"config_path", *configPath,
		"bind", cfg.Server.Bind,
		"azure_targets", len(cfg.AzureTargets),
		"clients", len(cfg.Clients),
	)

	authStore := auth.NewStore()
	if err := authStore.LoadFromConfig(cfg.Clients); err != nil {
		appLogger.Error("failed to initialize auth store", "error", err)
		os.Exit(1)
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
	router.Use(appmiddleware.MaxBodyBytes(cfg.Server.MaxRequestBodyBytes))

	router.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

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
	protected.Mount("/admin", admin.NewHandler(manager, authStore, proxyService, appLogger))
	protected.NotFound(proxyService.ServeHTTP)

	router.Mount("/", protected)

	server := &http.Server{
		Addr:              cfg.Server.Bind,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       time.Duration(cfg.Server.RequestTimeoutSeconds) * time.Second,
		WriteTimeout:      time.Duration(cfg.Server.RequestTimeoutSeconds) * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	appLogger.Info("http server starting", "addr", cfg.Server.Bind)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		appLogger.Error("http server stopped unexpectedly", "error", err)
		os.Exit(1)
	}
}
