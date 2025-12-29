# Project Context

## Purpose
Lightweight HTTP proxy service for unified forwarding of multiple Azure OpenAI endpoints. Centralizes credential management, request routing, structured logging, and provides automatic failover capabilities for internal clients accessing Azure resources through a single entry point.

## Tech Stack
- **Language**: Go 1.24.2
- **HTTP Framework**: github.com/go-chi/chi/v5
- **UUID**: github.com/google/uuid
- **Logging**: gopkg.in/natefinch/lumberjack.v2 (log rotation)
- **Standard Library**: log/slog (structured logging), context, net/http, sync, atomic

## Project Conventions

### Code Style
- Standard Go formatting (`go fmt`)
- No code comments unless explicitly requested
- Package-level comments minimal, code self-documenting
- Use `slog` for structured logging with key-value pairs
- Error wrapping with `fmt.Errorf` and `%w` verb
- Context propagation for request-scoped values (request IDs, authentication principal)

### Architecture Patterns
- **Layered architecture**: `cmd/` (entry) → `internal/` (core) with clear boundaries
- **Dependency injection**: Services receive dependencies via constructors
- **Interface segregation**: Public interfaces are minimal and focused
- **Concurrent-safe**: Use `sync.RWMutex`, `sync.Mutex`, and `atomic` for shared state
- **Graceful degradation**: Muted targets for failover, timeout handling
- **Middleware pattern**: Request ID generation, authentication logging

### Testing Strategy
- **Unit tests**: Standard `go test` with table-driven tests
- **Integration tests**: Run with `-tags=integration` flag via scripts
- **Coverage**: Tests cover config validation, auth store, proxy service logic
- **Test helpers**: Mock data structures in test files

### Git Workflow
- `make test` - Run unit tests
- `make integration` - Run integration tests
- `make build` - Build binary to `bin/azure-proxy`
- Feature work follows OpenSpec proposal workflow
- Commit changes only when explicitly requested

## Domain Context
- **Target**: Named Azure OpenAI endpoint with API key, resource path prefix
- **Client**: Authenticated principal with optional allowed_targets list
- **Muting**: Temporary target exclusion after consecutive failures (60s quiet period)
- **Forwarding**: HTTP request/response proxy with header filtering and streaming
- **Failover**: Automatic retry to alternate targets on retryable errors

## Important Constraints
- Client tokens must be valid and match configured credentials
- Target names are case-insensitive for lookup
- Muted targets excluded until quiet period expires
- Request timeout configurable (default 60s)
- Hop-by-hop headers stripped from proxy responses
- Authorization header removed, replaced with `api-key` header

## External Dependencies
- Azure OpenAI API endpoints (multiple, configured via JSON)
- No external database or service dependencies
- File-based configuration (JSON) with hot-reload support
- Log file rotation handled by lumberjack

## Key Files
- `cmd/proxy/main.go` - Application entry point
- `internal/proxy/service.go` - Core proxy logic and forwarding
- `internal/auth/middleware.go` - Authentication and principal handling
- `internal/config/config.go` - Configuration parsing and validation
- `config/config.json` - Runtime configuration template
