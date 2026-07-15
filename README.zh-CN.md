# CoreClaw MCP Server

CoreClaw MCP Server 将 CoreClaw OpenAPI v2 的公开接口暴露给 MCP 客户端，例如 Codex、Claude Desktop、Cursor、n8n，以及任何支持 stdio 或 Streamable HTTP MCP 的客户端。

## 托管入口

首选 MCP 入口是托管的 Streamable HTTP endpoint：

```text
https://mcp.coreclaw.com/mcp
```

当托管服务已经部署到本仓库当前版本后，客户端优先使用这个配置：

```json
{
  "mcpServers": {
    "coreclaw": {
      "url": "https://mcp.coreclaw.com/mcp",
      "headers": {
        "api-key": "your-coreclaw-token"
      }
    }
  }
}
```

服务入口支持 `api-key`、`X-API-Key` 或 `Authorization: Bearer <token>`，转发到 CoreClaw 上游时统一使用 `Authorization: Bearer <token>`。

## 范围

- API 事实来源：`exported-api-docs/openapi.json` 和 `exported-api-docs/endpoints.csv`
- 暴露为 MCP 工具的公开 v2 operation：37 个（34 个 OpenAPI operation + 3 个编排工具：`poll_run`、`verify_run`、`run_workers_batch`；`get_worker_run_log` 另增可选的进程内 `grep` 过滤）
- 排除的内部接口：`POST /api/v2/workers/{workerId}/versions`、`PUT /api/v2/workers/{workerId}/versions/{version}`、`GET /api/v2/workers/{workerId}/internal`
- 传输方式：stdio 和 Streamable HTTP
- REST 兼容入口：`POST /mcp/<tool_name>`
- 鉴权：入口支持 `api-key`、`X-API-Key`、`Authorization: Bearer <token>`，转发到 CoreClaw 时使用 `Authorization: Bearer <token>`
- Server instructions：在 MCP `initialize` 响应中返回中英文工作流指引
- Tool annotations：每个工具都显式声明 `title`、`readOnlyHint`、`destructiveHint`、`idempotentHint`、`openWorldHint`

## 分页补偿

CoreClaw 的列表接口（`list_store_workers`、`list_workers`、`list_worker_runs`、`list_worker_tasks` 及各 `list_*_results` 工具）**不**把 `offset` 当作绝对行偏移。后端把 `(offset, limit)` 转成 1 基分页（`page_index = floor(offset/limit) + 1`），因此 `offset=80, limit=100` 实际返回的是 `[0, 100)`，`offset=20, limit=50` 返回 `[0, 50)`。只有当 `offset` 是 `limit` 的整数倍时，`offset` 才与真实行偏移一致。

本 MCP 服务器做透明补偿：当请求的 `offset` 不是 `limit` 的整数倍时，工具会向上游发起按 `limit` 对齐的分页请求，再拼出精确的 `[offset, offset+limit)` 窗口，保证调用方始终拿到所请求的行。对齐的请求（含默认 `offset=0`）只走单次上游往返。调用方无需修改分页逻辑——任意 `offset`/`limit` 组合都能返回正确切片。这是对上游后端 bug 的客户端侧规避，已由单元测试与真机 API 回归测试覆盖。

## 构建和测试

```bash
go test ./...
go vet ./...
go build -o coreclaw-mcp-server .
```

真实 API 和本地 HTTP 验收：

```powershell
$env:CORECLAW_API_KEY="your-coreclaw-token"
.\scripts\verify-real-api.ps1
```

真实 MCP 触发的端到端运行验收：

```powershell
$env:CORECLAW_API_KEY="your-coreclaw-token"
.\scripts\verify-e2e-run.ps1
```

该脚本会启动本地 Streamable HTTP MCP 服务，通过 MCP `tools/call` 调用 `run_worker_task`，轮询 `get_worker_run`，并继续验证日志、结果行和 JSON 导出链接。

## 运行

stdio：

```bash
CORECLAW_API_KEY="your-coreclaw-token" ./coreclaw-mcp-server --transport stdio
```

HTTP：

```bash
./coreclaw-mcp-server --transport http --port 3000 --base-url https://openapi.coreclaw.com
```

HTTP 服务提供：

- `POST /mcp`：MCP Streamable HTTP
- `POST /mcp/<tool_name>`：REST 风格的单工具调用

## MCP 工具

本服务公开 37 个 v2 工具，其中 34 个与公开 endpoint 一一对应，另 3 个为编排工具（`poll_run`/`verify_run`/`run_workers_batch`，由自定义 handler 组合多个上游请求）。按模型实际使用链路排序：发现和预检、执行、查询运行、编排（轮询/验收）、读取结果/日志/导出、最后才是重跑或停止。

1. `list_store_workers`
2. `list_workers`
3. `get_worker` 或 `get_worker_input_schema`
4. `list_worker_tasks`，需要保存预设时用 `create_worker_task`，查看/修改/删除用 `get_worker_task`、`get_worker_task_input`、`update_worker_task`、`update_worker_task_input`、`delete_worker_task`
5. `run_worker` 或 `run_worker_task`；批量验收用 `run_workers_batch`
6. `get_worker_run`、`get_last_worker_run` 或 `get_worker_last_run`；等慢脚本结束用 `poll_run`，验收是否拿到有效数据用 `verify_run`
7. `list_worker_run_results` 或 `export_worker_run_results`
8. 失败排查用 `get_worker_run_log`（可传 `grep` 只看 Error/Traceback 行）
9. 明确要求重试时才使用 `rerun_*`，明确要求停止时才使用 `abort_*`

## MCP 配置示例

托管 HTTP：

```json
{
  "mcpServers": {
    "coreclaw": {
      "url": "https://mcp.coreclaw.com/mcp",
      "headers": {
        "api-key": "your-coreclaw-token"
      }
    }
  }
}
```

本地 stdio：

```json
{
  "mcpServers": {
    "coreclaw": {
      "command": "/absolute/path/to/coreclaw-mcp-server",
      "args": ["--transport", "stdio", "--base-url", "https://openapi.coreclaw.com"],
      "env": {
        "CORECLAW_API_KEY": "your-coreclaw-token"
      }
    }
  }
}
```

本地 HTTP：

```json
{
  "mcpServers": {
    "coreclaw": {
      "url": "http://localhost:3000/mcp",
      "headers": {
        "api-key": "your-coreclaw-token"
      }
    }
  }
}
```

## REST 兼容入口示例

```bash
curl -X POST http://localhost:3000/mcp/list_store_workers \
  -H "Content-Type: application/json" \
  -d '{"keyword":"amazon","offset":0,"limit":5}'

curl -X POST http://localhost:3000/mcp/get_account_info \
  -H "Content-Type: application/json" \
  -H "api-key: your-coreclaw-token" \
  -d '{}'

curl -X POST http://localhost:3000/mcp/run_worker \
  -H "Content-Type: application/json" \
  -H "api-key: your-coreclaw-token" \
  -d '{"worker_id":"YOUR_WORKER_ID","version":"v1.0.1","input_json":"{\"keyword\":\"coffee\",\"limit\":10}","is_async":true}'
```

`run_worker`、`create_worker_task`、`update_worker_task_input` 的 `input_json` 填从 `get_worker_input_schema` 看到的业务字段即可；MCP 服务会自动包装为 CoreClaw 实际使用的 `input.parameters.custom`。如果高级调用方要完全控制 CoreClaw 的 `input` 对象，可以改用 `raw_input_json`。

## GitHub 自动化部署

仓库包含：

- `.github/workflows/ci.yml`：格式检查、`go vet`、race 测试、构建
- `.github/workflows/release.yml`：多平台构建 artifact
- `.github/workflows/deploy.yml`：手动触发 SSH 部署到 Linux 服务器

建议配置 GitHub Secrets：

- `SSH_HOST`
- `SSH_USER`
- `SSH_PORT`（可选，默认 `22`）
- `SSH_PASSWORD`
- `CORECLAW_BASE_URL`（可选，默认 `https://openapi.coreclaw.com`）

不要提交 `.env`、API token 或服务器密码。
