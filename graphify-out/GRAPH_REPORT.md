# Graph Report - .  (2026-04-11)

## Corpus Check
- 73 files · ~0 words
- Verdict: corpus is large enough that graph structure adds value.

## Summary
- 712 nodes · 1125 edges · 49 communities detected
- Extraction: 62% EXTRACTED · 38% INFERRED · 0% AMBIGUOUS · INFERRED: 432 edges (avg confidence: 0.5)
- Token cost: 0 input · 0 output

## God Nodes (most connected - your core abstractions)
1. `Handler` - 33 edges
2. `newTestLogger()` - 28 edges
3. `testAuthClients()` - 28 edges
4. `writeJSON()` - 25 edges
5. `Handler` - 22 edges
6. `errorResponse()` - 14 edges
7. `SessionManager` - 14 edges
8. `CopilotService` - 13 edges
9. `Service` - 12 edges
10. `setupTestStores()` - 9 edges

## Surprising Connections (you probably didn't know these)
- None detected - all connections are within the same source files.

## Communities

### Community 0 - "Community 0"
Cohesion: 0.04
Nodes (18): AdminSessionConfig, AdminUser, Client, Config, DataFiles, DataStore, DefaultConfig(), IsValidEndpointType() (+10 more)

### Community 1 - "Community 1"
Cohesion: 0.11
Nodes (36): failingTransport, newTestLogger(), setupServiceTest(), setUpstream503RetryConfig(), testAuthClients(), TestGetToken(), TestGetToken_NonActiveAccount(), TestGetToken_Refresh() (+28 more)

### Community 2 - "Community 2"
Cohesion: 0.15
Nodes (7): errorResponse(), Handler, maskKey(), parseTimeValue(), parseUsageFilter(), toUsageCostTable(), writeJSON()

### Community 3 - "Community 3"
Cohesion: 0.08
Nodes (23): migrateAdminAudit(), migrateAdminUsers(), migrateClients(), MigrateFromJSON(), migrateModelCosts(), migrateUsageEvents(), extractUsageFromSSE(), extractUsageTokens() (+15 more)

### Community 4 - "Community 4"
Cohesion: 0.08
Nodes (10): CopilotService, deferCancel(), NewService(), normalizeRequestTimeout(), readAndBufferBody(), requestMetrics, Service, ServiceMetrics (+2 more)

### Community 5 - "Community 5"
Cohesion: 0.11
Nodes (5): copilotAccountResponse, Handler, replaceModelInBody(), sanitizeCopilotAccount(), Service

### Community 6 - "Community 6"
Cohesion: 0.08
Nodes (14): AddEventTotals(), AggregateResult, Bucket, CostRates, CostTable, DimensionTotals, EstimateEventCost(), Event (+6 more)

### Community 7 - "Community 7"
Cohesion: 0.11
Nodes (11): canonicalEndpointType(), Catalog, Cost, ModelEntry, New(), newFromData(), Config, Manager (+3 more)

### Community 8 - "Community 8"
Cohesion: 0.13
Nodes (21): classifyModel(), CopilotModelDetail, copilotModelDetailEntry, copilotModelEntry, copilotModelsAPIResponse, copilotModelsDetailAPIResponse, FetchModelDetails(), FetchModelsFromAPI() (+13 more)

### Community 9 - "Community 9"
Cohesion: 0.1
Nodes (13): DeviceCodeResponse, OAuthClient, OAuthTokenResponse, extractModel(), extractMultipartModel(), modelAllowed(), sanitizeRequestBodyForAzure(), Service (+5 more)

### Community 10 - "Community 10"
Cohesion: 0.18
Nodes (6): ContextWithSession(), generateSessionID(), Session, sessionContextKey, SessionManager, subtleConstantTimeStringCompare()

### Community 11 - "Community 11"
Cohesion: 0.15
Nodes (8): buildTargetStates(), newSelectionError(), newTargetState(), normalizePrefix(), selectionError, Service, targetState, TargetStats

### Community 12 - "Community 12"
Cohesion: 0.16
Nodes (11): AccessLogger(), authenticate(), contextKey, extractAccessKey(), extractAPIKeyFromQuery(), Middleware(), Recoverer(), remoteIP() (+3 more)

### Community 13 - "Community 13"
Cohesion: 0.18
Nodes (9): classifyTransportError(), copyHeaders(), forwardAttemptError, newStreamingWriter(), retryDelayWithJitter(), Service, sleepWithContext(), streamingWriter (+1 more)

### Community 14 - "Community 14"
Cohesion: 0.46
Nodes (14): seedClients(), setupTestStores(), testClients(), testConfig(), TestHandlerDataClientsCRUD(), TestHandlerHealthz(), TestHandlerMetrics(), TestHandlerReloadConfig() (+6 more)

### Community 15 - "Community 15"
Cohesion: 0.16
Nodes (7): gitHubCopilotUserResponse, quotaEntryCamel, quotaEntrySnake, QuotaInfo, QuotaManager, quotaSnapshotsCamel, quotaSnapshotsSnake

### Community 16 - "Community 16"
Cohesion: 0.27
Nodes (6): checkClientNameUnique(), clonePools(), CopilotPool, CopilotPoolStore, normalizePool(), validatePool()

### Community 17 - "Community 17"
Cohesion: 0.16
Nodes (3): buildMultipartBody(), TestConvertMultipartToJSON_FileMimeFromExtension(), TestConvertMultipartToJSON_SingleFile()

### Community 18 - "Community 18"
Cohesion: 0.19
Nodes (4): contains(), searchSubstring(), TestConfigValidateEndpointTypes(), TestConfigValidateInvalidEndpointType()

### Community 19 - "Community 19"
Cohesion: 0.27
Nodes (5): AuthenticateUser(), HashPassword(), HashPasswordWithRandomSalt(), UserStore, verifyPasswordHash()

### Community 20 - "Community 20"
Cohesion: 0.22
Nodes (6): setupTestDB(), TestSelectCopilotAccount_EmptyPool(), TestSelectCopilotAccount_FreeModelIgnoresQuota(), TestSelectCopilotAccount_NoAvailable(), TestSelectCopilotAccount_OrderBySort(), TestSelectCopilotAccount_SkipQuotaExhausted()

### Community 21 - "Community 21"
Cohesion: 0.27
Nodes (4): CopilotAccount, CopilotAccountStore, generateUUID(), isValidAccountStatus()

### Community 22 - "Community 22"
Cohesion: 0.26
Nodes (6): cloneCosts(), costKey(), ModelCost, ModelCostStore, normalizeCost(), validateCost()

### Community 23 - "Community 23"
Cohesion: 0.27
Nodes (3): loginPageData, Portal, sanitizeNext()

### Community 24 - "Community 24"
Cohesion: 0.24
Nodes (3): CopilotEndpoints, CopilotTokenResponse, TokenManager

### Community 25 - "Community 25"
Cohesion: 0.18
Nodes (1): recordCollector

### Community 26 - "Community 26"
Cohesion: 0.35
Nodes (5): checkAccessKeyUnique(), ClientStore, cloneClients(), normalizeClient(), validateClient()

### Community 27 - "Community 27"
Cohesion: 0.31
Nodes (9): _add_aliases(), convert_cost(), extract_capabilities(), main(), models.dev 中缺失但项目需要的模型条目（估算价格，仅供参考）。, 将 models.dev 的 cost ($/M tokens) 转为项目的 $/K tokens。, 从模型元数据中提取 capabilities 标签。, _supplementary_models() (+1 more)

### Community 28 - "Community 28"
Cohesion: 0.22
Nodes (0): 

### Community 29 - "Community 29"
Cohesion: 0.33
Nodes (5): createTestPool(), TestCopilotAccountStore_AutoSortOrder(), TestCopilotAccountStore_CRUD(), TestCopilotAccountStore_DefaultStatus(), TestCopilotAccountStore_ListByPool()

### Community 30 - "Community 30"
Cohesion: 0.25
Nodes (0): 

### Community 31 - "Community 31"
Cohesion: 0.29
Nodes (0): 

### Community 32 - "Community 32"
Cohesion: 0.33
Nodes (3): AuditEvent, auditKey(), AuditStore

### Community 33 - "Community 33"
Cohesion: 0.29
Nodes (2): affinityEntry, affinityMap

### Community 34 - "Community 34"
Cohesion: 0.6
Nodes (5): TestMigrateFromJSON(), TestMigrateIdempotent(), TestMigrateSkipsExisting(), writeJSONFile(), writeJSONLFile()

### Community 35 - "Community 35"
Cohesion: 0.33
Nodes (0): 

### Community 36 - "Community 36"
Cohesion: 0.4
Nodes (4): extractStreamField(), shouldAggregateSSE(), sseChunk, sseUsage

### Community 37 - "Community 37"
Cohesion: 0.6
Nodes (3): testDB(), TestEnsureValidToken_NotExpired(), TestEnsureValidToken_Refresh()

### Community 38 - "Community 38"
Cohesion: 0.5
Nodes (3): convertMultipartToJSON(), detectMimeType(), fileEntry

### Community 39 - "Community 39"
Cohesion: 0.5
Nodes (0): 

### Community 40 - "Community 40"
Cohesion: 0.5
Nodes (0): 

### Community 41 - "Community 41"
Cohesion: 0.5
Nodes (0): 

### Community 42 - "Community 42"
Cohesion: 0.5
Nodes (0): 

### Community 43 - "Community 43"
Cohesion: 0.83
Nodes (3): load_config(), main(), probe()

### Community 44 - "Community 44"
Cohesion: 1.0
Nodes (0): 

### Community 45 - "Community 45"
Cohesion: 1.0
Nodes (0): 

### Community 46 - "Community 46"
Cohesion: 1.0
Nodes (0): 

### Community 47 - "Community 47"
Cohesion: 1.0
Nodes (0): 

### Community 48 - "Community 48"
Cohesion: 1.0
Nodes (0): 

## Knowledge Gaps
- **58 isolated node(s):** `copilotAccountResponse`, `testStores`, `loginPageData`, `sessionContextKey`, `Session` (+53 more)
  These have ≤1 connection - possible missing edges or undocumented components.
- **Thin community `Community 44`** (2 nodes): `headers.go`, `ApplyEditorHeaders()`
  Too small to be a meaningful cluster - may be noise or needs more connections extracted.
- **Thin community `Community 45`** (2 nodes): `db.go`, `OpenDB()`
  Too small to be a meaningful cluster - may be noise or needs more connections extracted.
- **Thin community `Community 46`** (2 nodes): `affinity_test.go`, `TestAffinityMap()`
  Too small to be a meaningful cluster - may be noise or needs more connections extracted.
- **Thin community `Community 47`** (1 nodes): `ui_embed.go`
  Too small to be a meaningful cluster - may be noise or needs more connections extracted.
- **Thin community `Community 48`** (1 nodes): `run-integration-tests.ps1`
  Too small to be a meaningful cluster - may be noise or needs more connections extracted.

## Suggested Questions
_Questions this graph is uniquely positioned to answer:_

- **Are the 27 inferred relationships involving `newTestLogger()` (e.g. with `TestServiceFailoverOnTransportError()` and `TestServiceRejectsUnauthorizedTarget()`) actually correct?**
  _`newTestLogger()` has 27 INFERRED edges - model-reasoned connections that need verification._
- **Are the 27 inferred relationships involving `testAuthClients()` (e.g. with `TestServiceFailoverOnTransportError()` and `TestServiceRejectsUnauthorizedTarget()`) actually correct?**
  _`testAuthClients()` has 27 INFERRED edges - model-reasoned connections that need verification._
- **Are the 24 inferred relationships involving `writeJSON()` (e.g. with `.handleHealthz()` and `.handleMetrics()`) actually correct?**
  _`writeJSON()` has 24 INFERRED edges - model-reasoned connections that need verification._
- **What connects `copilotAccountResponse`, `testStores`, `loginPageData` to the rest of the system?**
  _58 weakly-connected nodes found - possible documentation gaps or missing edges._
- **Should `Community 0` be split into smaller, more focused modules?**
  _Cohesion score 0.04 - nodes in this community are weakly interconnected._
- **Should `Community 1` be split into smaller, more focused modules?**
  _Cohesion score 0.11 - nodes in this community are weakly interconnected._
- **Should `Community 3` be split into smaller, more focused modules?**
  _Cohesion score 0.08 - nodes in this community are weakly interconnected._