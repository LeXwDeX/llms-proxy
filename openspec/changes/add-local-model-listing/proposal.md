# Change: Serve configured models for /deployments and /models

## Why
- Clients expect to list available models/deployments via `/openai/deployments` or `/openai/models` just like Azure.
- Current proxy forwards upstream and enforces model requirements, causing 400s and showing unrelated Azure defaults.

## What Changes
- Intercept `GET /openai/deployments` and `GET /openai/models` to return the union of configured models (from `allowed_models`) in an Azure-compatible response structure.
- Do not require a model for these list calls and do not forward them upstream.

## Impact
- Affected specs: proxy-forwarding
- Affected code: proxy handler path handling, model validation bypass for list endpoints, docs/tests
