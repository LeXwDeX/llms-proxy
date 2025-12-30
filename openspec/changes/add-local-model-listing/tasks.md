## 1. Implementation
- [x] 1.1 Detect `GET /openai/deployments` and `GET /openai/models` and serve local responses without forwarding upstream.
- [x] 1.2 Build Azure-compatible list response using the union of configured `allowed_models` (or all models if allowlist empty) with deterministic ordering and `object: "list"` items of type `deployment`.
- [x] 1.3 Skip model-required checks for these list endpoints.
- [x] 1.4 Add tests covering deployments/models list responses, deduplication/order, and bypassing upstream/model gating.
- [x] 1.5 Update docs to note local model listing; run `go test ./...` and mark tasks complete.
