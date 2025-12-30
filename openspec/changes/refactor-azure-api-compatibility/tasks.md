## 1. Code Analysis and Planning
- [ ] 1.1 Map all supported Azure OpenAI API endpoints and their paths
- [ ] 1.2 Identify gaps in current proxy implementation (completions, embeddings, audio, images)
- [ ] 1.3 Review authentication flow and determine integration points for dual auth
- [ ] 1.4 Plan path routing strategy to handle /deployments/{deployment-id}/* patterns

## 2. Authentication Enhancement
- [ ] 2.1 Modify auth middleware to accept both `Authorization: Bearer {access_key}` and `api-key: {client_api_key}` headers
- [ ] 2.2 Update auth.Store to support dual credential lookup (access_key map and api_key map)
- [ ] 2.3 Add config validation for client api_key fields (optional, backward compatible)
- [ ] 2.4 Write tests for both authentication methods
- [ ] 2.5 Update logging to indicate which auth method was used

## 3. Path Routing Expansion
- [ ] 3.1 Update path matching logic to handle all Azure endpoints:
  - `/openai/deployments/{deployment-id}/completions`
  - `/openai/deployments/{deployment-id}/chat/completions`
  - `/openai/deployments/{deployment-id}/embeddings`
  - `/openai/deployments/{deployment-id}/audio/transcriptions`
  - `/openai/deployments/{deployment-id}/audio/translations`
  - `/openai/deployments/{deployment-id}/images/generations`
- [ ] 3.2 Ensure model extraction works for all endpoint types
- [ ] 3.3 Test path parsing with all endpoint patterns
- [ ] 3.4 Verify streaming responses work for all applicable endpoints

## 4. Request Forwarding Refinement
- [ ] 4.1 Review and simplify forwardRequest logic
- [ ] 4.2 Ensure header preservation is correct for all endpoint types
- [ ] 4.3 Verify multipart/form-data handling for audio endpoints
- [ ] 4.4 Test with actual Azure endpoints (integration tests)
- [ ] 4.5 Add error handling for unsupported endpoint types

## 5. Code Cleanup and Refactoring
- [ ] 5.1 Extract path/model extraction into separate functions
- [ ] 5.2 Consolidate duplicate header handling code
- [ ] 5.3 Improve error message clarity and consistency
- [ ] 5.4 Add comprehensive comments for complex logic paths
- [ ] 5.5 Review and simplify target selection logic if possible

## 6. Testing
- [ ] 6.1 Add unit tests for new authentication flows
- [ ] 6.2 Add unit tests for path matching across all endpoint types
- [ ] 6.3 Add integration tests for each Azure endpoint type
- [ ] 6.4 Test streaming responses for chat/completions endpoint
- [ ] 6.5 Test multipart/form-data for audio transcription/translation

## 7. Documentation
- [ ] 7.1 Update README with supported endpoints list
- [ ] 7.2 Document new authentication methods
- [ ] 7.3 Update config.json.example with new client fields
- [ ] 7.4 Add API contract documentation (if not exists)

## 8. Validation
- [ ] 8.1 Run all existing unit tests
- [ ] 8.2 Run all integration tests
- [ ] 8.3 Test backward compatibility with existing clients
- [ ] 8.4 Performance test with concurrent requests
- [ ] 8.5 Run openspec validation
