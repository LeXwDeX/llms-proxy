## 1. Implementation
- [x] 1.1 Update local `/openai/deployments` and `/openai/models` responses to match Azure list schema (list metadata and item fields) and validate against SDK expectations.
- [x] 1.2 Decide and implement behavior for targets with empty `allowed_models` (e.g., include/exclude), and align spec/code accordingly.
- [x] 1.3 Add/adjust tests for list format (including metadata) and allowlist-empty behavior.
- [x] 1.4 Update docs/specs to reflect the chosen model aggregation rule; run `go test ./...`.
