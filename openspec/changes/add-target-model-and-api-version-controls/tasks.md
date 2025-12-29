## 1. Implementation
- [x] 1.1 Extend Azure target config with `allowed_models` and `default_api_version`; validate that defaults are non-empty and model names are trimmed.
- [x] 1.2 Reject requests whose model (from path or body, as applicable) is not in the target's `allowed_models` when the allowlist is present.
- [x] 1.3 Rewrite outgoing requests to set `api-version` to the target's `default_api_version`, ignoring any client-provided value or absence.
- [x] 1.4 Update config sample/docs to show new fields; add unit tests covering model gating and api-version override.
- [x] 1.5 Run `go test ./...` and mark tasks complete.
