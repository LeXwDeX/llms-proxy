package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigValidateSuccess(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{
			Bind:                  "127.0.0.1:8080",
			RequestTimeoutSeconds: 30,
			MaxRequestBodyBytes:   1024,
		},
		AzureTargets: []AzureTarget{{
			Name:               "primary",
			Endpoint:           "https://example.com",
			ResourcePathPrefix: "/openai",
			AzureAPIKey:        "key",
		}},
		Clients: []Client{{
			Name:      "client",
			AccessKey: "secret",
		}},
		Logging: LoggingConfig{
			Level:     "info",
			AccessLog: "logs/access.log",
			ErrorLog:  "logs/error.log",
		},
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected no validation error, got %v", err)
	}
}

func TestConfigValidateErrors(t *testing.T) {
	cfg := &Config{
		Server:       ServerConfig{},
		AzureTargets: []AzureTarget{{}},
		Logging:      LoggingConfig{},
	}

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for incomplete configuration")
	}
}

func TestConfigCloneProducesDeepCopy(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{
			Bind:                  "0.0.0.0:8080",
			RequestTimeoutSeconds: 15,
			MaxRequestBodyBytes:   2048,
		},
		AzureTargets: []AzureTarget{{
			Name:               "primary",
			Endpoint:           "https://example.com",
			ResourcePathPrefix: "/openai",
			AzureAPIKey:        "key",
		}},
		Clients: []Client{{
			Name:           "team",
			AccessKey:      "abc",
			AllowedTargets: []string{"primary"},
		}},
		Logging: LoggingConfig{
			Level:     "debug",
			AccessLog: "logs/access.log",
			ErrorLog:  "logs/error.log",
		},
	}

	cloned := cfg.Clone()
	if cloned == cfg {
		t.Fatal("expected clone to allocate new struct")
	}

	cloned.Server.Bind = "127.0.0.1:9999"
	cloned.AzureTargets[0].Name = "secondary"
	cloned.Clients[0].AllowedTargets[0] = "secondary"

	if cfg.Server.Bind != "0.0.0.0:8080" {
		t.Errorf("original server bind mutated: %s", cfg.Server.Bind)
	}
	if cfg.AzureTargets[0].Name != "primary" {
		t.Errorf("original target mutated: %s", cfg.AzureTargets[0].Name)
	}
	if cfg.Clients[0].AllowedTargets[0] != "primary" {
		t.Errorf("original client allowed targets mutated: %s", cfg.Clients[0].AllowedTargets[0])
	}
}

func TestLoadReadsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	content := `{
		"server":{
			"bind":"0.0.0.0:8080",
			"request_timeout_seconds":10,
			"max_request_body_bytes":1024
		},
		"azure_targets":[{
			"name":"primary",
			"endpoint":"https://example.com",
			"resource_path_prefix":"/openai",
			"azure_api_key":"key"
		}],
		"clients":[{
			"name":"demo",
			"access_key":"token",
			"allowed_targets":["primary"]
		}],
		"logging":{
			"level":"info",
			"access_log":"logs/access.log",
			"error_log":"logs/error.log"
		}
	}`

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("expected load to succeed: %v", err)
	}

	if cfg.Server.Bind != "0.0.0.0:8080" {
		t.Errorf("unexpected bind: %s", cfg.Server.Bind)
	}
	if len(cfg.AzureTargets) != 1 || cfg.AzureTargets[0].Name != "primary" {
		t.Fatalf("unexpected targets: %#v", cfg.AzureTargets)
	}
}
