## MODIFIED Requirements
### Requirement: Use only server-configured Azure credentials
Targets SHALL forward requests to Azure using only server-configured credentials and SHALL NOT forward client-supplied Azure bearer tokens.

#### Scenario: Drop client Azure bearer
- **WHEN** a client sends Azure bearer credentials (for example `X-Azure-Authorization` or upstream `Authorization`)
- **THEN** the proxy SHALL strip them before forwarding and SHALL use the configured Azure credential instead.

#### Scenario: Require configured credential
- **WHEN** a target is configured without an Azure credential
- **THEN** configuration validation SHALL fail to prevent a credential-less target.

### Requirement: Target model allowlist
Targets SHALL be able to declare an allowlist of model deployment names and the proxy SHALL select targets based on the requested model.

#### Scenario: Model-first routing with load balancing
- **WHEN** a request references a model present in one or more targets’ `allowed_models`
- **THEN** the proxy SHALL select among those matching targets (respecting health/mute state) using a fair selection strategy and forward the request.

#### Scenario: Reject model not supported by any target
- **WHEN** no target’s `allowed_models` includes the requested model (or the model is missing while allowlists are configured)
- **THEN** the proxy SHALL return an error without forwarding the request upstream.

### Requirement: Enforce target default api-version
Each target SHALL define a default `api-version`, and the proxy SHALL enforce this value on every forwarded request, overriding any client-supplied value.

#### Scenario: Override client api-version
- **WHEN** a client sends a request with any `api-version` query parameter
- **THEN** the proxy SHALL replace it with the target's configured `default_api_version` before forwarding.

#### Scenario: Add api-version when missing
- **WHEN** a client sends a request without `api-version`
- **THEN** the proxy SHALL add the target's configured `default_api_version` before forwarding.
