# LynAI Backend

LynAI 后端服务，提供账号认证和插件市场 API。

## 技术栈

- Go 1.22+ / Gin / GORM
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
| `JWT_SECRET` | 是 | — | JWT 签名密钥，生产环境务必用随机长字符串 |
| `ADMIN_PASSWORD` | 是 | — | 管理员账号初始密码 |
| `PORT` | 否 | `8080` | 监听端口 |
| `STORAGE_DIR` | 否 | `./storage` | 插件 ZIP 存储目录 |
| `ADMIN_PHONE` | 否 | `0000000000` | 管理员手机号 |
| `ADMIN_DISPLAY_NAME` | 否 | `管理员` | 管理员显示名 |
| `ADMIN_TEMPLATES` | 否 | 可执行文件同目录 `templates/`，回退到 `internal/admin/templates` | Admin Web 面板模板目录 |

### 4. 启动

```bash
export DB_DSN="host=localhost port=5432 user=postgres dbname=lynai sslmode=disable"
export JWT_SECRET="your-random-secret-key"
export ADMIN_PASSWORD="your-admin-password"

make run
# 或直接运行编译产物
./bin/lynai-backend
```

首次启动会自动：
- 创建数据库表（GORM AutoMigrate）
- 创建管理员账号（默认手机号 `0000000000`，密码为你设置的 `ADMIN_PASSWORD`）

启动成功后输出 `LynAI backend listening on :8080`。

### 5. 验证

```bash
curl http://localhost:8080/health
# {"status":"ok"}
```

## API 端点

### 认证

| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| POST | `/auth/register` | 无 | 注册（phone + password + displayName?） |
| POST | `/auth/login` | 无 | 登录（phone + password），返回 access + refresh token |
| POST | `/auth/send-otp` | 无 | 预留短信验证码端点，当前返回 501 |
| POST | `/auth/verify-otp` | 无 | 预留短信验证端点，当前返回 501 |
| POST | `/auth/refresh` | 无 | 用 refresh token 换取新的 token pair |
| POST | `/auth/logout` | Bearer | 登出（客户端丢弃 token） |
| GET | `/auth/me` | Bearer | 获取当前用户信息 |

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

access token 有效期 15 分钟，过期后客户端自动用 refresh token 调 `/auth/refresh` 获取新 token，用户无感知。refresh token 有效期 30 天，30 天不活跃才需重新登录。

```bash
curl -X POST http://localhost:8080/auth/refresh \
  -H "Content-Type: application/json" \
  -d '{"refreshToken":"eyJ..."}'
```

### LynAI 中转

| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| POST | `/relay/chat` | Bearer | 登录用户调用 LynAI 中转；请求体为 OpenAI Chat Completions 兼容 JSON，并包含 `api_type`（如 `openai`）。服务端按 `api_type` 和 `model` 路由到管理员配置的上游，转发前会剥离 `api_type`。支持流式 SSE 透传。 |
| GET | `/relay/models` | Bearer | 返回可用模型列表，格式为 OpenAI 兼容 `object=list`，每个模型额外包含 `api_type`。 |
| GET | `/relay/config` | Bearer | 返回前端托管 Provider 同步所需的完整中转配置，按上游分组包含模型分类、能力和高级参数。 |
| POST | `/relay/transcribe` | Bearer | 转发 OpenAI 兼容音频转文字请求，multipart 字段包含 `model`、`api_type` 和 `file`；仅允许 `speech` 分类模型。 |
| POST | `/relay/images/generations` | Bearer | 转发 OpenAI 兼容图片生成请求，JSON body 包含 `model`、`api_type` 和 `prompt`；仅允许 `image_generation` 分类模型。 |

上游 Provider 由管理员在 `/admin/relay` 配置，包含名称、Endpoint、API Type、API Key、模型列表和启用状态。当前 MVP 仅实现 OpenAI 兼容上游；计费、额度和协议翻译留待后续上线。

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

ZIP 包内必须包含 `plugin.json` 文件，字段如下：

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

管理面板用 cookie 认证（HttpOnly，30 天有效期），与 API 的 Bearer token 互相独立。面板 POST 表单使用双层 CSRF token 校验；HTTPS 请求下 cookie 自动启用 Secure 属性。管理员 cookie 在剩余有效期低于 7 天时自动续期。

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
make test     # 运行测试（用 SQLite 内存数据库，不需要 PostgreSQL）
make vet      # 静态检查
make fmt      # 格式化
make clean    # 清理编译产物
```

### 测试

测试使用 SQLite 内存数据库，不需要连接 PostgreSQL：

```bash
make test
```

测试覆盖：
- 注册、登录、重复注册、错误密码、OTP 预留端点
- Token 刷新、无效 refresh token、refresh token 不能用于 API
- 插件提交、列表只显示已上架、批准/驳回流程
- 下载、我的提交、更新检查
- 权限隔离（非管理员不能审核、无 token 不能提交）

## 部署

### systemd

仓库提供了 `deploy/lynai-backend.service`。部署时复制到 systemd 目录，并在服务器上的副本里替换 `CHANGE-ME-*` 占位值：

```ini
sudo cp deploy/lynai-backend.service /etc/systemd/system/lynai-backend.service
sudo nano /etc/systemd/system/lynai-backend.service
```

service 文件内包含所有必需环境变量：

- `DB_DSN` — PostgreSQL 连接串
- `JWT_SECRET` — JWT 签名密钥，生产环境务必使用随机长字符串
- `ADMIN_PASSWORD` — 初始管理员密码
- `PORT` — 监听端口，默认 `8080`
- `STORAGE_DIR` — 插件 ZIP 和同步 blob 存储目录
- `ADMIN_PHONE` — 初始管理员手机号，默认 `0000000000`
- `ADMIN_DISPLAY_NAME` — 初始管理员显示名，默认 `管理员`
- `ADMIN_TEMPLATES` — Admin Web 模板目录

完整部署示例：

```bash
make build

sudo useradd -r -s /usr/sbin/nologin lynai || true
sudo mkdir -p /opt/lynai-backend/bin /opt/lynai-backend/storage
sudo cp bin/lynai-backend /opt/lynai-backend/bin/lynai-backend
sudo rm -rf /opt/lynai-backend/templates
sudo cp -r internal/admin/templates /opt/lynai-backend/templates
sudo chown -R lynai:lynai /opt/lynai-backend
sudo cp deploy/lynai-backend.service /etc/systemd/system/lynai-backend.service
sudo nano /etc/systemd/system/lynai-backend.service

sudo systemctl daemon-reload
sudo systemctl enable lynai-backend
sudo systemctl start lynai-backend
```

首次启动会用 `ADMIN_PHONE` / `ADMIN_DISPLAY_NAME` / `ADMIN_PASSWORD` 创建第一个管理员。之后访问 `http://<host>:8080/admin/login`，用该手机号和密码登录面板。

### Nginx 反向代理（可选）

```nginx
server {
    listen 443 ssl;
    server_name api.lynai.com;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        client_max_body_size 50M;  # 插件 ZIP 上传
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
