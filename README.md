# LynAI Backend

LynAI 后端服务，提供账号认证和插件市场 API。

## 技术栈

- Go 版本以 `go.mod` 为准 / Gin / GORM
- PostgreSQL 14+
- JWT（access 15 分钟 + refresh 30 天）
- bcrypt 密码哈希

## 快速开始

### 1. 准备 PostgreSQL

```bash
# Debian/Ubuntu
sudo apt install postgresql
sudo -u postgres psql -c "CREATE DATABASE lynai;"

# 或用已有的 PostgreSQL 实例，只需创建一个空数据库即可
```

### 2. 编译

```bash
make build
```

产出二进制 `bin/lynai-backend`。

### 3. 配置环境变量

| 变量 | 必填 | 默认值 | 说明 |
|------|------|--------|------|
| `DB_DSN` | 是 | — | PostgreSQL 连接串，如 `host=localhost port=5432 user=postgres dbname=lynai sslmode=disable` |
| `JWT_SECRET` | 是 | — | JWT 签名密钥，至少 32 bytes，必须使用随机值，拒绝示例或默认占位值 |
| `ADMIN_PASSWORD` | 是 | — | 管理员账号初始密码 |
| `PORT` | 否 | `8080` | 监听端口 |
| `STORAGE_DIR` | 否 | `./storage` | 插件 ZIP 存储目录 |
| `ADMIN_PHONE` | 否 | `0000000000` | 管理员手机号 |
| `ADMIN_DISPLAY_NAME` | 否 | `管理员` | 管理员显示名 |
| `MACHINE_ID` | 是 | — | Server 模式必需的 Snowflake 节点 ID，范围 0-1023；多实例必须各不相同。`migrate` 子命令不需要 |
| `SNOWFLAKE_ROLLBACK_TIMEOUT` | 否 | `5s` | 系统时钟回退时等待恢复的最长时间；超时后的新账号请求返回 HTTP 503 |
| `RELAY_PRIVATE_HOST_ALLOWLIST` | 否 | 空 | 允许 relay 使用 HTTP 和访问私网地址的上游 host，逗号分隔；支持 `host` 或精确的 `host:port`，例如 `localhost:11434` |
| `SYNC_CLOCK_SKEW` | 否 | `5m` | 签名时间戳允许偏差，Go duration，范围 `(0, 1h]` |
| `SYNC_REPLAY_RETENTION` | 否 | `24h` | 精确响应重放保存期，至少等于 clock skew，至多 `720h` |
| `ADMIN_SESSION_TTL` | 否 | `720h` | Admin opaque 服务端会话 TTL；活跃会话在剩余四分之一时原位续期 |
| `SESSION_CLEANUP_INTERVAL` | 否 | `1h` | user/admin/speech 过期会话清理周期 |
| `RELAY_SPEECH_SESSION_TTL` | 否 | `2h` | 长语音共享数据库会话的滑动 TTL |
| `RELAY_SPEECH_PER_USER_CAPACITY` | 否 | `5` | 每个用户同时占用的长语音 session/reservation 上限 |
| `RELAY_SPEECH_GLOBAL_CAPACITY` | 否 | `500` | 全局长语音 session/reservation 上限 |
| `RELAY_NON_STREAM_TIMEOUT` | 否 | `2m` | relay 非流式请求从连接到读完响应的总超时 |
| `RELAY_STREAM_IDLE_TIMEOUT` | 否 | `45s` | relay 流式响应两次读取之间的最大空闲时间 |
| `RELAY_STREAM_MAX_DURATION` | 否 | `30m` | relay 单次流式响应的最长持续时间 |

### 4. 启动

```bash
export DB_DSN="host=localhost port=5432 user=postgres dbname=lynai sslmode=disable"
export JWT_SECRET="$(openssl rand -base64 48)"
export ADMIN_PASSWORD="your-admin-password"
export MACHINE_ID="REPLACE_WITH_A_UNIQUE_ID_FROM_0_TO_1023"

make run
# 或直接运行编译产物
./bin/lynai-backend
```

首次启动前先显式运行数据库迁移：

```bash
make migrate
# 或
./bin/lynai-backend migrate
```

迁移使用内嵌、版本化 PostgreSQL SQL，在 `schema_migrations` 中保存 SHA-256 校验和，并使用 PostgreSQL advisory lock 防止多实例并发执行。普通启动只校验数据库已应用全部已知版本且校验和一致，不会运行 `AutoMigrate` 或修改表结构。迁移完成后的首次普通启动会创建管理员账号（默认手机号 `0000000000`，密码为你设置的 `ADMIN_PASSWORD`）。如果该手机号已属于普通用户，启动会明确报错且不会将其提升为管理员。

systemd 示例从 `/etc/lynai-backend/environment` 读取必需配置；文件不存在时 systemd 不会启动服务，缺少 `MACHINE_ID` 等必需变量时程序会报错退出。请显式填写每个实例唯一的 `MACHINE_ID`，不要复制复用示例节点 ID。

启动成功后输出 `LynAI backend listening on :8080`。

### 5. 验证

```bash
curl http://localhost:8080/health
# {"status":"ok"}
```

## API 端点

### 安全同步协议 v1

设备身份、canonical binary encoding、enrollment，以及 `/sync/changes` 和 blob 上传的 Ed25519 签名与幂等语义见 [`../lynai/doc/protocol-v1.md`](../lynai/doc/protocol-v1.md)。所有同步写接口都强制要求当前登录 session 下已登记设备的有效签名，不提供 unsigned fallback。`GET /sync/status` 返回服务端当前执行的 change batch、change data、请求体、分页和 blob 大小 limits；changes/blob 请求体超限返回 HTTP 413，changes JSON 必须只有一个顶层值且不允许尾随 JSON。`GET /sync/changes` 的兼容字段 `latestSeq` 是本页安全游标（等于 `nextSince`），全局高水位另见 `globalLatestSeq`，因此旧客户端即使忽略 `hasMore` 也不会跳过 capped page。`GET /sync/blobs` 始终是有界列表；客户端必须在 `hasMore`/`truncated` 为 true 时使用 `nextAfter` 继续请求，`returnedCount` 和 `pageSize` 可用于检测旧版单页截断。

同步 blob 是每用户的 content-addressed 文件。共享同一 POSIX `STORAGE_DIR` 的 PostgreSQL 多实例通过数据库 transaction advisory lock 串行化同一用户/hash 的发布和损坏清理；SQLite 测试使用进程内锁。上传内容通过 SHA-256 验证后才发布，若同路径已有损坏文件会原子替换；下载发现内容 hash 不符会删除损坏文件及其 metadata 并返回 not found。

同步变更 allowlist 包含版本化的 `shared_settings`、`synced_model_configs`、`plugin_files`、`plugin_settings` 和 `plugin_config` 逻辑域。运行时 `plugin_storage` 有意保持设备本地，不进入云同步。客户端只上传共享设置投影和用户明确启用的非托管 Provider 非秘密配置；完整 `app_settings`、API key、secret 引用和后端托管 Relay 配置不属于云同步 payload。

### 认证

| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| POST | `/auth/register` | 无 | 注册（phone + password + displayName?） |
| POST | `/auth/login` | 无 | 登录（phone + password），返回 access + refresh token |
| POST | `/auth/send-otp` | 无 | 预留短信验证码端点，当前返回 501 |
| POST | `/auth/verify-otp` | 无 | 预留短信验证端点，当前返回 501 |
| POST | `/auth/refresh` | 无 | 用 refresh token 换取新的 token pair |
| POST | `/auth/revoke` | 无 | 用 refresh token 幂等撤销其服务端 session；无效或已撤销 token 也返回 204 |
| POST | `/auth/logout` | Bearer | 用当前 access token 撤销服务端 session |
| GET | `/auth/me` | Bearer | 获取当前用户信息 |

### 设备注册

| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| POST | `/devices/challenge` | Bearer | 签发绑定当前用户和登录 session 的 5 分钟注册 challenge |
| POST | `/devices/enroll` | Bearer | 用 Ed25519 公钥和签名注册当前设备 |
| GET | `/devices` | Bearer | 列出当前用户设备 |
| GET | `/devices/current` | Bearer | 获取当前登录 session 注册的有效设备 |
| PATCH | `/devices/:id` | Bearer | 重命名当前用户的有效设备 |
| DELETE | `/devices/:id` | Bearer | 撤销当前用户的有效设备 |

challenge、公钥和签名使用严格、无 padding 的 base64url。客户端先向 `/devices/challenge` 提交拟注册的 `deviceId`、Ed25519 公钥、显示名称、平台和协议版本；服务端把这些字段与认证用户/session 一起绑定到只保存 SHA-256 摘要的 5 分钟一次性 challenge。客户端随后按 [`protocol-v1.md`](../lynai/doc/protocol-v1.md) 构造 domain-separated CBE1 enrollment 消息并签名。设备 ID 是完整公钥 SHA-256 的 52 字符小写无 padding Base32；同一 backend origin 下的不同账号必须使用独立身份，服务端全局拒绝跨账号复用 device ID 或公钥。重复注册同一账号下未撤销的身份是幂等操作。

迁移 `0005_device_identity_global_unique.sql` 会为 device ID 和公钥增加全局唯一索引。若历史数据库中已存在跨账号重复身份，迁移会明确失败；应先让受影响客户端升级到账号作用域身份、重新登记设备并清理旧重复记录，再重新运行迁移。

**注册请求示例：**

```bash
curl -X POST http://localhost:8080/auth/register \
  -H "Content-Type: application/json" \
  -d '{"phone":"13800001111","password":"secret123","displayName":"Alice"}'
```

**响应：**

```json
{
  "user": {
    "id": "uuid",
    "phone": "13800001111",
    "displayName": "Alice",
    "avatarUrl": null,
    "email": null,
    "isAdmin": false
  },
  "token": {
    "accessToken": "eyJ...",
    "refreshToken": "eyJ...",
    "expiresAt": 1782818837438
  }
}
```

**Token 刷新：**

access token 有效期 15 分钟，过期后客户端自动用 refresh token 调 `/auth/refresh` 获取新 token pair。refresh token 每次使用后立即轮换，旧 token 不能重放；登出会撤销服务端会话，管理员降权也会立即对现有 token 生效。引入服务端会话表的版本会使升级前签发的旧 token 失效，用户需要重新登录一次。

```bash
curl -X POST http://localhost:8080/auth/refresh \
  -H "Content-Type: application/json" \
  -d '{"refreshToken":"eyJ..."}'
```

### LynAI 中转

新版客户端优先使用 `/relay/v2/*`。旧版 `/relay/*` 端点继续保留；客户端连接不支持 v2 的旧后端时会自动回退。

| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| GET | `/relay/v2/config` | Bearer | 返回 schemaVersion 2 的分类 Provider 配置。一个响应 Provider 只包含同一分类的模型。 |
| POST | `/relay/v2/chat` | Bearer | 统一托管 Chat 入口，根据 `provider_id`、`api_type` 和 `model` 转发到 OpenAI、Anthropic 或 Ollama 上游。 |
| POST | `/relay/v2/ocr` | Bearer | v2 OCR 入口。 |
| POST | `/relay/v2/transcribe` | Bearer | v2 语音转文字入口。 |
| POST | `/relay/v2/speech/*` | Bearer | v2 长语音会话入口。 |
| POST | `/relay/v2/images/generations` | Bearer | v2 图片生成入口。 |

v2 Chat 请求示例：

```json
{
  "provider_id": "1",
  "api_type": "anthropic",
  "model": "claude-sonnet",
  "messages": [
    {"role": "user", "content": "hi"}
  ],
  "stream": true
}
```

`provider_id` 和 `api_type` 仅用于 LynAI 后端路由，转发上游前会被移除。v2 统一的是 LynAI 入口路径；请求和响应正文仍采用对应上游协议格式。

| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| POST | `/relay/chat` | Bearer | 登录用户调用 LynAI 中转；请求体为 OpenAI Chat Completions 兼容 JSON，并包含 `api_type`（如 `openai`）。服务端按 `api_type` 和 `model` 路由到管理员配置的上游，转发前会剥离 `api_type`。支持流式 SSE 透传。 |
| GET | `/relay/models` | Bearer | 返回可用模型列表，格式为 OpenAI 兼容 `object=list`，每个模型额外包含 `api_type`。 |
| GET | `/relay/config` | Bearer | 返回前端托管 Provider 同步所需的完整中转配置，按上游分组包含模型分类、能力和高级参数。 |
| POST | `/relay/transcribe` | Bearer | 转发 OpenAI 兼容音频转文字请求，multipart 字段包含 `model`、`api_type` 和 `file`；仅允许 `speech` 分类模型。 |
| POST | `/relay/images/generations` | Bearer | 转发 OpenAI 兼容图片生成请求，JSON body 包含 `model`、`api_type` 和 `prompt`；仅允许 `image_generation` 分类模型。 |

上游 Provider 由管理员在 `/admin/relay` 配置，包含名称、Endpoint、API Type、API Key、模型列表和启用状态。当前 MVP 仅实现 OpenAI 兼容上游；计费、额度和协议翻译留待后续上线。

Relay 上游默认必须使用 HTTPS。解析后的 loopback、private、link-local、multicast 和 unspecified IP 会在建立连接前被拒绝，且上游重定向被禁止。仅本机 Ollama 等明确可信的私网上游可加入 `RELAY_PRIVATE_HOST_ALLOWLIST`；配置 `localhost:11434` 后可使用 `http://localhost:11434`，配置 `localhost` 则允许该 host 的任意端口。不要把不受信任或宽泛的内部域名加入 allowlist。

`/relay/models` 和 `/relay/config` 中的模型条目会包含 LynAI 扩展字段：`category`（`chat` / `ocr` / `speech` / `image_generation`）、`displayName`、`description`、`capabilities`（`vision`、`thinking`、`tools`）、`advancedParams`（如 `maxTokens`、`temperature`、`topP`、`presencePenalty`、`frequencyPenalty`、`seed`、`stop`、`user`、`debugSse`）、`providerId` 和 `providerName`。Chat 端点允许 `chat` 和 `ocr` 分类模型；语音和图片端点只允许各自分类模型。

**中转请求示例：**

```bash
curl -X POST http://localhost:8080/relay/chat \
  -H "Authorization: Bearer <accessToken>" \
  -H "Content-Type: application/json" \
  -d '{"api_type":"openai","model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"stream":true}'
```

### 插件市场

| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| GET | `/market/plugins` | 无 | 浏览已上架插件（`?category=&q=&page=1&page_size=20`） |
| GET | `/market/plugins/:id` | 无 | 插件详情 |
| GET | `/market/plugins/:id/download` | 无 | 下载插件 ZIP |
| POST | `/market/updates` | Bearer | 批量检查已安装插件是否有更新 |
| POST | `/market/plugins/submit` | Bearer | 提交插件到市场（multipart 上传 ZIP） |
| GET | `/market/submissions/mine` | Bearer | 查看我的提交及审核状态 |
| GET | `/market/plugins/pending` | Admin | 待审核插件列表 |
| POST | `/market/plugins/:id/approve` | Admin | 批准插件上架 |
| POST | `/market/plugins/:id/reject` | Admin | 驳回插件（body: `{"reason":"..."}`） |

**浏览插件：**

```bash
curl "http://localhost:8080/market/plugins?q=test&page=1&page_size=20"
```

**提交插件：**

```bash
curl -X POST http://localhost:8080/market/plugins/submit \
  -H "Authorization: Bearer <accessToken>" \
  -F "zip=@my-plugin.zip"
```

提交 ZIP 的净文件大小上限为 16 MiB；multipart 请求体上限会额外预留协议开销。ZIP 根目录必须且只能包含一个规范路径 `plugin.json`，不接受嵌套、重复、反斜线或 `.` / `..` 等非规范路径。版本必须是完整 SemVer（例如 `1.2.3`）。客户端本地安装 ZIP 的 32 MiB 安全上限是独立限制，不代表市场提交上限。

`plugin.json` 字段如下：

```json
{
  "id": "my-plugin",
  "name": "My Plugin",
  "version": "1.0.0",
  "author": "your-name",
  "description": "插件简介",
  "permissions": ["network", "storage"]
}
```

**检查更新：**

```bash
curl -X POST http://localhost:8080/market/updates \
  -H "Authorization: Bearer <accessToken>" \
  -H "Content-Type: application/json" \
  -d '{"installed":[{"id":"my-plugin","version":"0.9.0"}]}'
```

### 插件审核流程

```
用户提交 ZIP → status: pending
  → 管理员 approve → status: approved → 出现在公开市场列表
  → 管理员 reject  → status: rejected  → 用户可修改后重新提交
```

公开的 `/market/plugins` 只返回 `approved` 状态的插件。提交者可通过 `/market/submissions/mine` 查看自己的审核状态。

## Admin Web 面板

浏览器访问 `http://localhost:8080/admin` 即可使用管理面板，无需安装 app。

- 用管理员账号登录
- 概览页：待审核数、已上架数、注册用户数
- 待审核页：查看插件详情、批准上架、驳回（可填理由）
- 全部插件页：查看所有插件，进入详情页后可编辑元数据、下架、删除
- 中转上游页：配置 LynAI 中转使用的上游 Endpoint、API Type、API Key 和模型列表
- 用户页：查看用户列表、提升/取消管理员、创建新管理员账号

管理面板使用独立的 opaque cookie；数据库只保存 token 的 SHA-256 摘要，不复用 App refresh JWT。会话默认 30 天有效，活跃时原位滑动续期，因此并发请求不会因 token 轮换互相登出。面板 POST 表单使用双层 CSRF token 校验；HTTPS 请求下 cookie 自动启用 Secure 属性。

长语音 session 保存在 PostgreSQL，可由多实例共享。创建上游任务前会先写入容量 reservation，reservation 与已创建 session 一起计入 per-user/global 上限；上游创建失败会释放 reservation，过期记录由后台清理。

Relay 非流式调用采用总超时，流式调用采用 idle timeout 和 max duration。响应尚未开始时超时返回 OpenAI 风格 `504 upstream_timeout`；流已经发送后只能终止连接，调用日志会记录 `upstream_timeout`。

## 开发

### 项目结构

```
cmd/server/main.go              # 入口
internal/
  config/                       # 环境变量配置
  database/                     # GORM 连接 + 模型定义
  auth/                         # JWT、注册/登录/刷新、中间件
  market/                       # 市场查询、提交、审核、存储
  relay/                        # 登录用户可用的 LynAI 中转
  admin/                        # Admin Web 面板（HTML 模板）
  server/                       # 路由注册
  testutil/                     # 测试工具（SQLite 内存数据库）
storage/plugins/                # 插件 ZIP 文件存储
```

### 常用命令

```bash
make build    # 编译
make run      # 编译并运行
make migrate  # 显式应用 PostgreSQL 迁移
make test     # 运行单元测试；未设置 TEST_POSTGRES_DSN 时跳过 PostgreSQL 集成测试
make vet      # 静态检查
make fmt      # 格式化
make clean    # 清理编译产物
```

### 测试

默认测试使用 SQLite 内存数据库，不需要连接 PostgreSQL；PostgreSQL 集成测试在未设置 `TEST_POSTGRES_DSN` 时自动跳过：

```bash
make test
```

要在本地运行完整集成测试，将 `TEST_POSTGRES_DSN` 指向一个测试专用 PostgreSQL 数据库。每个测试会在随机 schema 中应用内嵌迁移，并在结束时删除该 schema：

```bash
TEST_POSTGRES_DSN='postgres://postgres:postgres@127.0.0.1:5432/lynai_test?sslmode=disable' go test ./...
```

CI 提供 PostgreSQL service 并设置 `TEST_POSTGRES_DSN`，因此会强制运行这些集成测试。集成测试覆盖内嵌迁移、`ValidateSchema`、并发迁移 advisory lock，以及真实 PostgreSQL 设备唯一约束冲突的错误映射。

测试覆盖：
- 注册、登录、重复注册、错误密码、OTP 预留端点
- Token 刷新、无效 refresh token、refresh token 不能用于 API
- 插件提交、列表只显示已上架、批准/驳回流程
- 下载、我的提交、更新检查
- 权限隔离（非管理员不能审核、无 token 不能提交）

## 部署

### systemd

仓库提供了 `deploy/lynai-backend.service`。该 unit 通过 `EnvironmentFile=/etc/lynai-backend/environment` 读取必需配置。先创建仅 root 可读的配置文件：

```bash
sudo install -d -o root -g root -m 700 /etc/lynai-backend
sudo touch /etc/lynai-backend/environment
sudo chown root:root /etc/lynai-backend/environment
sudo chmod 600 /etc/lynai-backend/environment
sudoedit /etc/lynai-backend/environment
```

`/etc/lynai-backend/environment` 必须包含以下值；每个运行实例使用不同的 `MACHINE_ID`：

```ini
DB_DSN=host=/var/run/postgresql port=5432 user=lynai dbname=lynai sslmode=disable
JWT_SECRET=<粘贴 openssl rand -base64 48 的输出>
ADMIN_PASSWORD=REPLACE_WITH_A_STRONG_PASSWORD
MACHINE_ID=REPLACE_WITH_A_UNIQUE_ID_FROM_0_TO_1023
```

确认文件所有权和权限没有被编辑器改变：

```bash
sudo chown root:root /etc/lynai-backend/environment
sudo chmod 600 /etc/lynai-backend/environment
```

必需环境变量：

- `DB_DSN` — PostgreSQL 连接串
- `JWT_SECRET` — 至少 32 bytes 的随机 JWT 签名密钥；可用 `openssl rand -base64 48` 生成
- `ADMIN_PASSWORD` — 初始管理员密码
- `MACHINE_ID` — Snowflake 节点 ID，范围 0-1023，多实例必须各不相同

service 文件提供以下可选变量的默认值：

- `PORT` — 监听端口，默认 `8080`
- `STORAGE_DIR` — 插件 ZIP 和同步 blob 存储目录
- `ADMIN_PHONE` — 初始管理员手机号，默认 `0000000000`
- `ADMIN_DISPLAY_NAME` — 初始管理员显示名，默认 `管理员`
- `RELAY_PRIVATE_HOST_ALLOWLIST` — 可选的 relay 私网上游 host 或 host:port 逗号列表；默认空

Admin Web 模板已编译进二进制，不需要部署单独的模板目录。

完整部署示例：

```bash
make build

sudo useradd -r -s /usr/sbin/nologin lynai || true
sudo mkdir -p /opt/lynai-backend/bin /opt/lynai-backend/storage
sudo install -o lynai -g lynai -m 755 bin/lynai-backend /opt/lynai-backend/bin/lynai-backend
sudo chown -R lynai:lynai /opt/lynai-backend
sudo cp deploy/lynai-backend.service /etc/systemd/system/lynai-backend.service

# 使用 /etc/lynai-backend/environment 中配置的同一 DB_DSN，先显式应用迁移
sudo -u lynai env DB_DSN='host=/var/run/postgresql port=5432 user=lynai dbname=lynai sslmode=disable' /opt/lynai-backend/bin/lynai-backend migrate

sudo systemctl daemon-reload
sudo systemctl enable lynai-backend
sudo systemctl start lynai-backend
```

首次启动会用 `ADMIN_PHONE` / `ADMIN_DISPLAY_NAME` / `ADMIN_PASSWORD` 创建第一个管理员。之后访问 `http://<host>:8080/admin/login`，用该手机号和密码登录面板。

### 更新与回滚

管理员页面已内置在二进制中，更新时只需构建、替换二进制并重启：

```bash
git pull origin master
go test ./...
make build

sudo cp /opt/lynai-backend/bin/lynai-backend /opt/lynai-backend/bin/lynai-backend.previous
sudo install -o lynai -g lynai -m 755 bin/lynai-backend /opt/lynai-backend/bin/lynai-backend
sudo -u lynai env DB_DSN='host=/var/run/postgresql port=5432 user=lynai dbname=lynai sslmode=disable' /opt/lynai-backend/bin/lynai-backend migrate
sudo systemctl restart lynai-backend
curl --fail http://127.0.0.1:8080/health
```

更新失败时恢复旧二进制：

```bash
sudo systemctl stop lynai-backend
sudo cp /opt/lynai-backend/bin/lynai-backend.previous /opt/lynai-backend/bin/lynai-backend
sudo chown lynai:lynai /opt/lynai-backend/bin/lynai-backend
sudo chmod 755 /opt/lynai-backend/bin/lynai-backend
sudo systemctl start lynai-backend
```

旧版本部署留下的 `/opt/lynai-backend/templates` 不再使用，确认新版本正常后可以删除。

### Nginx 反向代理（可选）

```nginx
server {
    listen 443 ssl;
    server_name api.lynai.com;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-Proto $scheme;
        client_max_body_size 65M;  # 必须不小于 64 MiB blob 上限，并留出协议开销
    }
}
```

## 前端对接

LynAI Flutter app 通过设置页「后端连接」输入后端地址，app 会自动切换到远端服务：

- `RemoteAccountService` — 调用 `/auth/*` 端点
- `RemoteMarketService` — 调用 `/market/*` 端点
- `ModelConfigProvider.syncLynaiManagedProvider` — 调用 `/relay/config` 同步托管 LynAI 中转模型，旧服务端无该端点时回退 `/relay/models`

access token 过期时 `BackendClient` 自动刷新并重试，用户无感知。

未配置后端地址时，账号登录不可用；插件市场仍可使用本地桩显示未连接空态。
# Plugin Sync Domains

The sync change allowlist includes `plugin_files`, `plugin_settings`, and `plugin_config`. Runtime `plugin_storage` is intentionally device-local and is not transferable. Plugin bytes use the existing per-user content-addressed `/sync/blobs/:sha256` storage, so identical files are deduplicated and metadata changes remain incremental/tombstone-capable.
