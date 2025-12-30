# Change: Refactor for Azure API compatibility and multi-source authentication

## Why
- The proxy should be a transparent bridge to Azure OpenAI, supporting all Azure API endpoints and maintaining request/response format consistency
- Current implementation only handles chat/completions paths but Azure supports completions, embeddings, audio (transcriptions/translations), and image generation
- Clients need flexibility to authenticate using Azure-style credentials (api-key header) in addition to proxy-specific access_key
- Code organization needs cleanup to maintain clarity as functionality grows

## What Changes
- Support all Azure OpenAI API endpoints: completions, chat/completions, embeddings, audio/transcriptions, audio/translations, images/generations
- Add flexible client authentication: accept both proxy `access_key` (Bearer) and Azure `api-key` header for proxy clients
- Ensure request path forwarding preserves Azure URL structure (/openai/deployments/{deployment-id}/*)
- Maintain backward compatibility with existing clients using Bearer access_key
- Code cleanup: extract routing logic, consolidate authentication flow, improve error handling clarity

## Impact
- Affected specs: proxy-forwarding
- Affected code: auth middleware, proxy service, path matching logic, config schema (optional new fields), tests
- No breaking changes to existing client APIs; new authentication mode is additive
