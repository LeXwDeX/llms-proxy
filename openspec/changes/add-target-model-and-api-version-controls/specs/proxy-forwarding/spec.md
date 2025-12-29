## ADDED Requirements
### Requirement: Target model allowlist
Targets SHALL be able to declare an allowlist of model deployment names and the proxy SHALL reject requests targeting models outside that list.

#### Scenario: Reject model not allowed
- **WHEN** a request references a model (in path or payload) not included in the target's `allowed_models`
- **THEN** the proxy SHALL return an error without forwarding the request upstream.

#### Scenario: Allow configured model
- **WHEN** a request references a model present in the target's `allowed_models`
- **THEN** the proxy SHALL forward the request normally.

### Requirement: Enforce target default api-version
Each target SHALL define a default `api-version`, and the proxy SHALL enforce this value on every forwarded request, overriding any client-supplied value.

#### Scenario: Override client api-version
- **WHEN** a client sends a request with any `api-version` query parameter
- **THEN** the proxy SHALL replace it with the target's configured `default_api_version` before forwarding.

#### Scenario: Add api-version when missing
- **WHEN** a client sends a request without `api-version`
- **THEN** the proxy SHALL add the target's configured `default_api_version` before forwarding.
