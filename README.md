# OmniApi

Go 实现的模型网关，一份代码同时交付 Windows 托盘桌面应用和 Linux 容器服务。控制台仍为 Vue 3 + Arco Design，构建产物通过 `//go:embed` 打进二进制。对外提供 OpenAI Chat Completions、OpenAI Responses 与 Anthropic Messages 三种入口，对上游同样支持这三种协议。

## 运行形态

| 模式 | 命令 | 说明 |
| --- | --- | --- |
| 桌面 | `omni-api.exe`（默认 `--mode desktop`） | 常驻系统托盘，默认监听 `127.0.0.1:47831`，托盘菜单“打开配置”可打开浏览器 |
| 服务 | `omni-api --mode server --host 0.0.0.0 --port 47831 --data-dir /data` | 无 GUI，适合容器；非 Windows 构建不链接任何桌面依赖 |

参数与环境变量：`--mode`/`OMNI_MODE`、`--host`/`OMNI_HOST`、`--port`/`OMNI_PORT`、`--data-dir`/`OMNI_DATA_DIR`、`OMNI_MASTER_KEY`、`OMNI_ADMIN_TOKEN`、`OMNI_STATIC_DIR`、`OMNI_WEB_URL`。

## 开发启动

```powershell
npm install
npm run dev
```

`npm run dev` 并行启动 Vite（`http://127.0.0.1:5173/`，`/api` 默认代理到 `http://127.0.0.1:47832/`）和开发 Go 网关（`http://127.0.0.1:47832/`）。只跑开发后端用 `npm run dev:go`。日常桌面服务仍使用默认端口 `47831`，可与开发环境同时运行；设置 `OMNI_API_SERVER` 可覆盖 Vite 的代理目标。端口隔离不代表数据隔离，两个实例如使用同一数据目录仍会共享配置和数据库。

## 构建

```powershell
npm run go:build
```

该命令先把控制台构建到 `internal/webui/dist`，再产出内嵌前端的 `omni-api.exe`（约 12 MB）。导入离线 Linux 镜像后，在 `docker-compose.yml` 所在目录启动：

```bash
docker load -i omni-api-v0.0.1-docker-amd64.tar.gz
export OMNI_MASTER_KEY='请替换为强密码'
export OMNI_ADMIN_TOKEN='请替换为管理 Token'
mkdir -p data
docker compose up -d
```

运行数据保存在当前目录的 `data/` 中。容器入口会自动修正目录权限；CentOS/RHEL 的 Compose 挂载使用 `:Z` 处理 SELinux 标签。

## 配置顺序

1. 在“鉴权密钥”中创建外部调用使用的访问密钥。
2. 在“供应商”中录入 Base URL、协议和 API Key，点击“同步上游模型”，再从上游返回的列表中勾选需要接入的模型；不支持模型列表接口的供应商仍可手动添加。
3. 在供应商的上游模型列表中为需要改名的模型填写别名。别名为空时对外暴露模型 ID；填写别名后仅暴露别名，原模型 ID 不再出现在模型列表也不可调用。
4. 在“概览”查看最终对外暴露的模型。暴露名称相同的上游模型自动组成一个负载均衡组，按供应商顺序、再按供应商内模型顺序轮询。

供应商 API Key 和访问令牌以 AES-GCM 加密写入 SQLite 数据库 `<data-dir>/omni-api.db`，管理 API 不回传密钥明文。数据库使用 `settings`、`providers` 和 `provider_models` 表保存配置与有序模型。首次启动时，若数据库尚无配置且同目录存在旧的 `<data-dir>/config.json`，程序会导入其中的加密字段；仅在导入成功后将旧文件重命名为 `config.json.migrated`。密钥来源为 `OMNI_MASTER_KEY`，未设置时自动生成 `<data-dir>/secret.key`。容器部署务必显式设置 `OMNI_MASTER_KEY`，否则重建卷后旧配置无法解密。

管理 Token 保存在加密 SQLite 配置中。空数据库首次启动时可通过 `OMNI_ADMIN_TOKEN` 初始化；已有管理 Token 不会被环境变量覆盖。设置后管理接口强制校验。代理接口使用已创建的访问密钥。绑定到 `0.0.0.0` 且任一鉴权配置为空时，启动日志会打印告警。

## 外部调用

请求使用 `Authorization: Bearer <访问密钥>`，请求体中的 `model` 填概览里的暴露名称。`GET /v1/models` 同样需要访问密钥，返回去重并升序排列的暴露名称。

```text
POST http://127.0.0.1:47831/v1/chat/completions
POST http://127.0.0.1:47831/v1/responses
POST http://127.0.0.1:47831/v1/messages
```

每次请求从该暴露名称的下一个上游模型开始轮询，只有启用的供应商参与。网络错误、超时、`401/403/404/408/429/5xx` 会尝试下一个；`400/409/422` 会立即返回。全部失败时返回最后一次上游错误，并带 `all_providers_failed` 和 `requestId`。

## 运行日志

控制台侧栏和 Windows 托盘菜单均提供“运行日志”入口。页面每秒自动刷新，显示最近 22 条日志，点击请求 ID 可查看同一次请求的各个阶段。

日志包含请求进入、请求解析、上游转发、失败重试、流式响应开始和最终结果；读取请求体、等待上游或流式传输超过 10 秒时，持续记录等待时长。网络错误会保留连接、TLS 或超时原因，流式读取失败会返回错误事件并记录失败。

运行日志只在内存保留本次启动的最近 1,000 条，页面显示其中最近 22 条，重启后清空。已结束的请求仍保存到“请求监控”。日志不主动记录请求正文或鉴权头，已配置的密钥会脱敏，上游地址不包含用户名、密码和查询参数。管理接口 `GET /api/v1/runtime-logs` 使用与其他管理接口相同的鉴权规则。

## MVP 协议范围

- 支持跨协议文本消息、system、temperature、top-p、最大输出 token 和非流式文本响应。
- 支持三种协议的文本 SSE 输入与输出转换；只有发送首个下游事件前允许切换供应商。
- 工具定义会传递到上游，但跨协议工具调用、工具结果和流式工具参数尚未完成统一转换。
- 图片、音频、推理状态、prompt caching、citations、conversation/store 等协议专属能力暂未支持。

## 验证

```powershell
go build ./...
go vet ./...
npm run go:test
npm run web:check
```

上述命令均应成功完成后再进行版本发布。
