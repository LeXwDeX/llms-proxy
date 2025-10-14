# Internal Training Outline

The following agenda can be used to onboard new engineers or operators responsible for the Azure OpenAI proxy.

## 1. Architecture Overview (30 min)
- Review repository layout (`cmd`, `internal`, `config`, `logs`).
- Explain the request flow: client → middleware → auth → proxy → Azure target.
- Demonstrate failover behaviour when the primary target is unreachable.

## 2. Hands-On Configuration (45 min)
- Clone the repository and build the binary with `make build`.
- Walk through the provided `config/config.json` and adjust values for the sandbox environment.
- Walk through client token management and `allowed_targets`.
- Use `/admin/config/reload` to apply changes without restart.

## 3. Operations Toolkit (30 min)
- Inspect logs under `logs/`.
- Use `/admin/metrics` and `/admin/healthz` to understand current load and target health.
- Practise the rollback procedure described in `docs/operations.md`.

## 4. Testing and Validation (30 min)
- Run unit tests with `make test`.
- Run integration tests (`scripts/run-integration-tests.sh`) to observe automatic failover.
- Discuss how to add new unit tests when extending features.

## 5. Incident Drills (30 min)
- Simulate invalid client token usage and resolve.
- Disable a target endpoint to observe mute windows and recovery.
- Role-play response to elevated 5xx responses from Azure, including escalation steps.

## 6. Security & Compliance (15 min)
- Reinforce secure storage of Azure API keys and client tokens.
- Review access control recommendations (dedicated management token, least privilege).
- Describe audit expectations: log retention, access log review cadence.

Provide this document to trainees ahead of the live session and capture Q&A for future revisions.
