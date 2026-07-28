# LynAI Backend 协作说明

## 验证

- 基础门禁：`go test ./...`、`go vet ./...`、`go build ./...`。
- Relay 流协议或并发变化追加 `go test -race ./internal/relay`。
- Migration、constraint、index、sequence 或 advisory lock 变化必须使用测试 PostgreSQL 执行 `TEST_POSTGRES_DSN='postgres://...' go test ./...`。

## Canonical Relay

- Flutter 客户端拥有 Agent loop 和工具调度；Go 后端只负责认证、路由、Provider 适配、canonical stream 和隐私安全日志，不在 Relay 中重建 Agent Runtime。
- `event: chunk` 是兼容边界。新增字段必须保持向后兼容；tool call、reasoning、completion marker、usage 或 error 语义变化必须同时更新 adapter、fixture、测试、README 和客户端 `doc/protocol-v1.md`。
- canonical stream 的 `sequence` 在单次响应内单调递增，`responseId` 在流内稳定；包含终态 tool calls 的 delta 使用 `type: tool_calls`，普通终态使用 `completed`，错误使用 `error`。
- OpenAI streaming usage 通过 `stream_options.include_usage` 请求；用户 `extraParams` 不能覆盖 relay 管理的 model、messages、stream、tools 或 generation 字段。
- 下游客户端断开属于 `client_disconnect`，不得冷却健康上游或计入上游失败；真正的 Provider/协议错误才参与路由 cooldown 和失败统计。
- 日志和响应不得泄漏 API key、JWT、Secret、Authorization header、完整 Prompt、Tool arguments 或上游敏感正文。

## 安全与路由

- Relay 上游默认 HTTPS，私网或 HTTP 只能通过精确 allowlist。禁止重定向和 URL 凭据，并保留 DNS rebinding 防护。
- Search 上游 origin 只能来自服务端配置，客户端仅可选择固定 provider ID；保持 HTTPS 默认、精确私网 allowlist、禁止重定向、DNS rebinding 防护和 secret-safe 错误。
- 请求、上传、SSE frame 和上游响应必须在解析或大内存分配前执行大小限制。
- 普通 server 启动只验证 schema，不自动迁移；migration 必须使用连续新文件，不修改已发布 migration。
