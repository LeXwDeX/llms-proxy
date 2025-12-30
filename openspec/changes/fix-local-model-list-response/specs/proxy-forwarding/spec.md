## MODIFIED Requirements
### Requirement: Local model listing endpoints
The proxy SHALL serve configured models for list endpoints instead of forwarding upstream.

#### Scenario: List deployments
- **WHEN** a client sends `GET /openai/deployments` (any api-version)
- **THEN** the proxy SHALL return an Azure-compatible list response built from configured models and SHALL NOT require a model or forward upstream.

#### Scenario: List models
- **WHEN** a client sends `GET /openai/models`, `GET /v1/models`, or `GET /models` (any api-version)
- **THEN** the proxy SHALL return an Azure-compatible list response built from configured models and SHALL NOT require a model or forward upstream.

#### Scenario: Include configured models
- **WHEN** building the list response
- **THEN** the proxy SHALL include the union of configured `allowed_models` across targets with non-empty allowlists, de-duplicated, deterministic, and shaped like Azure deployments (`object: "deployment"`, `id`, `model`, plus standard list metadata fields).
