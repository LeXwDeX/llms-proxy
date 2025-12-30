## ADDED Requirements
### Requirement: Azure-style proxy authentication
The proxy SHALL accept client access keys using the same headers/parameters Azure supports, without extra proxy-specific headers.

#### Scenario: Authenticate with bearer header
- **WHEN** a client sends `Authorization: Bearer <access_key>`
- **THEN** the proxy SHALL authenticate the request using that value.

#### Scenario: Authenticate with api-key header or query
- **WHEN** a client sends `api-key: <access_key>` in the header or as `api-key` query parameter
- **THEN** the proxy SHALL authenticate the request using that value.

#### Scenario: Strip client auth before forwarding
- **WHEN** forwarding upstream
- **THEN** the proxy SHALL remove client auth headers/params and use the configured Azure credential for the target.
