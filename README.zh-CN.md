# CoreClaw MCP Server

CoreClaw MCP Server 将 CoreClaw OpenAPI v2 的公开接口暴露给 MCP 客户端，例如 Codex、Claude Desktop、Cursor、n8n，以及任何支持 stdio 或 Streamable HTTP MCP 的客户端。

## 范围

- API 事实来源：`exported-api-docs/openapi.json` 和 `exported-api-docs/endpoints.csv`
- 暴露为 MCP 工具的公开 v2 operation：28 个
- 排除的内部接口：`POST /api/v2/workers/{workerId}/versions`、`PUT /api/v2/workers/{workerId}/versions/{version}`、`GET /api/v2/workers/{workerId}/internal`
- 传输方式：stdio 和 Streamable HTTP
- REST 兼容入口：`POST /mcp/<tool_name>`
- 鉴权：入口支持 `api-key`、`X-API-Key`、`Authorization: Bearer <token>`，转发到 CoreClaw 时使用 `Authorization: Bearer <token>`

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

本服务公开 28 个 v2 工具，名称与公开 endpoint 一一对应。完整表见英文 README；常用链路如下：

1. `list_store_workers`
2. `get_worker_input_schema`
3. `run_worker`
4. `get_worker_run` 或 `get_last_worker_run`
5. `list_worker_run_results` 或 `export_worker_run_results`
6. 失败排查用 `get_worker_run_log`

## MCP 配置示例

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

远程 HTTP：

```json
{
  "mcpServers": {
    "coreclaw": {
      "url": "https://your-server.example.com/mcp",
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

`run_worker` 的 `input_json` 填从 `get_worker_input_schema` 看到的业务字段即可；MCP 服务会自动包装为 CoreClaw 实际使用的 `input.parameters.custom`。如果高级调用方要完全控制 CoreClaw 的 `input` 对象，可以改用 `raw_input_json`。

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
