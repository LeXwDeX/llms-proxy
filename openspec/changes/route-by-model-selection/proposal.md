# Change: Route requests by model with target load balancing

## Why
- Clients expect the configured access_key to authenticate directly, and model selection should drive which Azure endpoint is used.
- Current target-first routing rejects valid models when the chosen endpoint’s allowlist doesn’t include them; we need model-first selection and load balancing across endpoints that support the requested model.

## What Changes
- Authenticate clients using the exact `access_key` from config (no overrides), as today.
- Select targets by the requested model: find endpoints whose `allowed_models` include the model and pick among them (load balance when multiple match); return an error if none match.
- Keep target selection deterministic/fair when multiple endpoints support the same model.

## Impact
- Affected specs: proxy-forwarding
- Affected code: target selection logic, model allowlist handling, tests, docs/config notes
