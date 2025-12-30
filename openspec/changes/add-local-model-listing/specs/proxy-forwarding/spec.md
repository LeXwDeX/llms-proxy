## ADDED Requirements
### Requirement: Local model listing endpoints
The proxy SHALL serve configured models for list endpoints instead of forwarding upstream.

#### Scenario: List deployments
- **WHEN** a client sends `GET /openai/deployments` (any api-version)
- **THEN** the proxy SHALL return an Azure-compatible list response built from configured models and SHALL NOT require a model or forward upstream.

#### Scenario: List models
- **WHEN** a client sends `GET /openai/models` (any api-version)
- **THEN** the proxy SHALL return an Azure-compatible list response built from configured models and SHALL NOT require a model or forward upstream.

#### Scenario: Include configured models
- **WHEN** building the list response
- **THEN** the proxy SHALL include the union of configured `allowed_models` across all targets (or all models if allowlist is empty), de-duplicated and in deterministic order, with items shaped like Azure deployments (`object: "deployment"`, `id`, `model`).
