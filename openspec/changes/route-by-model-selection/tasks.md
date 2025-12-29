## 1. Implementation
- [x] 1.1 Update target selection to choose endpoints by requested model: gather targets whose `allowed_models` contain the model (or all targets if allowlist empty), error when no target supports the model.
- [x] 1.2 Add load balancing/fair selection across matching targets (e.g., round-robin) while respecting muted/health state.
- [x] 1.3 Ensure authentication uses exact `access_key` values from config; document expected client header format.
- [x] 1.4 Update tests (model routing, multi-target load balancing, no-match error) and docs/config guidance.
- [x] 1.5 Run `go test ./...` and mark tasks complete.
