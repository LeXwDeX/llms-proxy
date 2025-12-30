## 1. Implementation
- [x] 1.1 Update auth middleware to accept proxy access keys via `Authorization: Bearer <access_key>` and `api-key: <access_key>` (header or query), matching Azure client behavior.
- [x] 1.2 Ensure upstream forwarding strips client auth headers and uses configured Azure credentials.
- [x] 1.3 Add tests for bearer/api-key header/query auth success/failure.
- [x] 1.4 Update docs to state “call exactly like Azure; only the key value differs.”
- [x] 1.5 Run `go test ./...` and mark tasks complete.
