## ADDED Requirements

### Requirement: Support all Azure OpenAI API endpoints
The proxy SHALL forward requests to all supported Azure OpenAI API endpoints without modification to the request path or response format.

#### Scenario: Forward completions request
- **WHEN** a client sends a POST request to `/openai/deployments/{deployment-id}/completions`
- **THEN** the proxy SHALL forward the request to the selected Azure target preserving the path, headers, and body
- **AND** SHALL return the Azure response unchanged

#### Scenario: Forward chat completions request
- **WHEN** a client sends a POST request to `/openai/deployments/{deployment-id}/chat/completions`
- **THEN** the proxy SHALL forward the request to the selected Azure target preserving the path, headers, and body
- **AND** SHALL support streaming responses

#### Scenario: Forward embeddings request
- **WHEN** a client sends a POST request to `/openai/deployments/{deployment-id}/embeddings`
- **THEN** the proxy SHALL forward the request to the selected Azure target preserving the path, headers, and body

#### Scenario: Forward audio transcription request
- **WHEN** a client sends a POST request to `/openai/deployments/{deployment-id}/audio/transcriptions` with multipart/form-data
- **THEN** the proxy SHALL forward the request to the selected Azure target preserving the multipart body

#### Scenario: Forward audio translation request
- **WHEN** a client sends a POST request to `/openai/deployments/{deployment-id}/audio/translations` with multipart/form-data
- **THEN** the proxy SHALL forward the request to the selected Azure target preserving the multipart body

#### Scenario: Forward image generation request
- **WHEN** a client sends a POST request to `/openai/deployments/{deployment-id}/images/generations`
- **THEN** the proxy SHALL forward the request to the selected Azure target preserving the path, headers, and body

#### Scenario: Reject unsupported endpoint
- **WHEN** a client sends a request to an unsupported path
- **THEN** the proxy SHALL return a 400 error with a message indicating the endpoint is not supported

### Requirement: Flexible client authentication
The proxy SHALL support client authentication using both proxy-specific Bearer tokens and Azure-style api-key headers.

#### Scenario: Authenticate with Bearer access_key
- **WHEN** a client provides `Authorization: Bearer {access_key}` header
- **THEN** the proxy SHALL authenticate using the configured access_key from clients configuration
- **AND** SHALL proceed with the request if valid

#### Scenario: Authenticate with api-key header
- **WHEN** a client provides `api-key: {client_api_key}` header
- **THEN** the proxy SHALL authenticate using the client's api_key from configuration
- **AND** SHALL proceed with the request if valid
- **AND** SHALL prioritize this auth method when both headers are present

#### Scenario: Reject missing authentication
- **WHEN** a client provides neither `Authorization: Bearer` nor `api-key` header
- **THEN** the proxy SHALL return 401 Unauthorized with appropriate WWW-Authenticate headers

#### Scenario: Reject invalid credential
- **WHEN** a client provides an invalid access_key or api_key
- **THEN** the proxy SHALL return 401 Unauthorized

## MODIFIED Requirements

### Requirement: Use server-configured Azure credentials
Targets SHALL forward requests to Azure using only server-configured credentials and SHALL NOT forward client-supplied Azure credentials.

#### Scenario: Drop client Authorization header
- **WHEN** a client sends an `Authorization` header (after client authentication is verified)
- **THEN** the proxy SHALL strip it before forwarding to Azure
- **AND** SHALL use the target's configured azure_api_key instead

#### Scenario: Set target api-key header
- **WHEN** forwarding a request to an Azure target
- **THEN** the proxy SHALL set the `api-key` header to the target's configured azure_api_key
- **AND** SHALL ensure this header is set even if the client authenticated with api-key

### Requirement: Target model allowlist
Targets SHALL be able to declare an allowlist of model deployment names and the proxy SHALL select targets based on the requested model.

#### Scenario: Extract model from all endpoint paths
- **WHEN** a request arrives at any Azure endpoint path (completions, chat/completions, embeddings, audio, images)
- **THEN** the proxy SHALL extract the deployment-id from the path `/openai/deployments/{deployment-id}/...`
- **AND** SHALL use it as the model identifier for target selection

#### Scenario: Model-first routing with load balancing
- **WHEN** a request references a deployment-id present in one or more targets' `allowed_models`
- **THEN** the proxy SHALL select among those matching targets (respecting health/mute state) using a fair selection strategy and forward the request

#### Scenario: Reject model not supported by any target
- **WHEN** no target's `allowed_models` includes the requested deployment-id
- **THEN** the proxy SHALL return an error without forwarding the request upstream

### Requirement: Enforce target default api-version
Each target SHALL define a default `api-version`, and the proxy SHALL enforce this value on every forwarded request, overriding any client-supplied value.

#### Scenario: Override client api-version for all endpoints
- **WHEN** a client sends a request to any Azure endpoint with any `api-version` query parameter
- **THEN** the proxy SHALL replace it with the target's configured `default_api_version` before forwarding

#### Scenario: Add api-version when missing
- **WHEN** a client sends a request to any Azure endpoint without `api-version`
- **THEN** the proxy SHALL add the target's configured `default_api_version` before forwarding
