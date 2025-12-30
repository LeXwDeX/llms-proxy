# Change: Fix local model list format and allowlist handling

## Why
- Current `/openai/deployments` and `/openai/models` local responses may be incompatible with Azure SDK expectations (missing list metadata).
- Spec said “include all models if allowlist empty” but implementation skips targets with empty `allowed_models`.

## What Changes
- Align local list responses (deployments/models) with Azure list format, including standard list metadata fields to satisfy SDKs.
- Clarify/adjust model collection behavior: decide and implement how targets with empty `allowed_models` contribute to the list (spec and code consistent).

## Impact
- Affected specs: proxy-forwarding (local model listing)
- Affected code: proxy list handlers, model aggregation, tests/docs
