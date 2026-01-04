## 1. Implementation
- [ ] 明确 capability id 与范围（更新 `proposal.md` 与 delta spec 路径）
- [ ] 补齐需求：在 `specs/feature-tbd/spec.md` 写清 Requirements/Scenarios
- [ ] 实现核心逻辑（按需求拆分到对应 `internal/*` 模块）
- [ ] 配置与校验（如涉及 `internal/config`）
- [ ] 运维接口/指标/日志（如涉及 `internal/admin` 或 `internal/logging`）

## 2. Tests
- [ ] 单元测试：覆盖关键分支与错误路径（`go test ./...`）
- [ ] 集成测试：覆盖端到端行为（`go test -tags=integration ./test/...` 或脚本）

## 3. Docs
- [ ] 更新 `docs/api-contract.md`（如对外接口有变化）
- [ ] 更新 `docs/operations.md` / `docs/docker-deploy.md`（如部署或运维有变化）
- [ ] 更新 `README.md`（如新增功能影响使用方式）

## 4. Validation
- [ ] `openspec validate add-your-feature-here --strict`
- [ ] `make test` / `make integration`（视改动范围）

