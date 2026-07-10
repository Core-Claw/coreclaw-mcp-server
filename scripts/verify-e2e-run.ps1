param(
  [string]$BaseUrl = $(if ($env:CORECLAW_BASE_URL) { $env:CORECLAW_BASE_URL } else { "https://openapi.coreclaw.com" }),
  [string]$ApiToken = $env:CORECLAW_API_KEY,
  [int]$Port = 3301,
  [string]$WorkerTaskId = $env:CORECLAW_E2E_WORKER_TASK_ID,
  [int]$PollIntervalSeconds = 5,
  [int]$PollTimeoutSeconds = 180,
  [switch]$SkipBuild,
  [switch]$DirectWorkerRun,
  [string]$WorkerId = $(if ($env:CORECLAW_E2E_WORKER_ID) { $env:CORECLAW_E2E_WORKER_ID } else { "01KT3DB41H2AXNEWP9SNJE5KKC" }),
  [string]$WorkerVersion = $(if ($env:CORECLAW_E2E_WORKER_VERSION) { $env:CORECLAW_E2E_WORKER_VERSION } else { "v1.0.1" }),
  [string]$WorkerInputJson = $(if ($env:CORECLAW_E2E_WORKER_INPUT_JSON) { $env:CORECLAW_E2E_WORKER_INPUT_JSON } else { '{"collection_type":"hashtag","targets":[{"string":"fyp"}],"fetch_count":1,"fetch_comments":false}' })
)

$ErrorActionPreference = "Stop"
Add-Type -AssemblyName System.Net.Http

if ([string]::IsNullOrWhiteSpace($ApiToken)) {
  throw "CORECLAW_API_KEY is required for real end-to-end verification."
}

function Invoke-JsonRpc {
  param(
    [int]$Port,
    [string]$Body,
    [string]$SessionId = "",
    [string]$ApiToken = ""
  )

  $handler = [System.Net.Http.HttpClientHandler]::new()
  $client = [System.Net.Http.HttpClient]::new($handler)
  try {
    $request = [System.Net.Http.HttpRequestMessage]::new([System.Net.Http.HttpMethod]::Post, "http://127.0.0.1:$Port/mcp")
    $request.Content = [System.Net.Http.StringContent]::new($Body, [System.Text.Encoding]::UTF8, "application/json")
    if (-not [string]::IsNullOrWhiteSpace($SessionId)) {
      $request.Headers.TryAddWithoutValidation("Mcp-Session-Id", $SessionId) | Out-Null
    }
    if (-not [string]::IsNullOrWhiteSpace($ApiToken)) {
      $request.Headers.TryAddWithoutValidation("api-key", $ApiToken) | Out-Null
    }

    $response = $client.SendAsync($request).GetAwaiter().GetResult()
    $text = $response.Content.ReadAsStringAsync().GetAwaiter().GetResult()
    if (-not $response.IsSuccessStatusCode) {
      throw "MCP HTTP request failed with status $([int]$response.StatusCode): $text"
    }
    $sid = ""
    if ($response.Headers.Contains("Mcp-Session-Id")) {
      $sid = ($response.Headers.GetValues("Mcp-Session-Id") | Select-Object -First 1)
    }
    return [pscustomobject]@{ Body = $text; SessionId = $sid }
  }
  finally {
    $client.Dispose()
    $handler.Dispose()
  }
}

function ConvertFrom-ToolText {
  param([object]$JsonRpcResponse)

  $payload = $JsonRpcResponse.Body | ConvertFrom-Json
  if ($payload.error) {
    throw "MCP JSON-RPC error: $($payload.error | ConvertTo-Json -Depth 20 -Compress)"
  }
  if ($payload.result.isError) {
    $message = ""
    if ($payload.result.content -and $payload.result.content.Count -gt 0) {
      $message = $payload.result.content[0].text
    }
    throw "MCP tool returned error: $message"
  }
  if (-not $payload.result.content -or $payload.result.content.Count -eq 0) {
    return $null
  }
  $text = $payload.result.content[0].text
  if ([string]::IsNullOrWhiteSpace($text)) {
    return $null
  }
  return ($text | ConvertFrom-Json)
}

function Invoke-McpTool {
  param(
    [int]$Port,
    [string]$SessionId,
    [string]$ApiToken,
    [string]$Name,
    [hashtable]$Arguments = @{}
  )

  if (-not $script:NextJsonRpcId) {
    $script:NextJsonRpcId = 10
  }
  $script:NextJsonRpcId += 1

  $body = @{
    jsonrpc = "2.0"
    id = $script:NextJsonRpcId
    method = "tools/call"
    params = @{
      name = $Name
      arguments = $Arguments
    }
  } | ConvertTo-Json -Depth 100 -Compress

  $response = Invoke-JsonRpc -Port $Port -SessionId $SessionId -ApiToken $ApiToken -Body $body
  return ConvertFrom-ToolText $response
}

function Get-RunId {
  param([object]$RunResponse)

  foreach ($field in @("run_slug", "slug", "run_id", "id")) {
    if ($RunResponse.PSObject.Properties.Name -contains $field) {
      $value = $RunResponse.$field
      if (-not [string]::IsNullOrWhiteSpace("$value")) {
        return "$value"
      }
    }
  }
  throw "Run response did not include a run identifier: $($RunResponse | ConvertTo-Json -Depth 20 -Compress)"
}

function Wait-RunSucceeded {
  param(
    [int]$Port,
    [string]$SessionId,
    [string]$ApiToken,
    [string]$RunId,
    [int]$PollIntervalSeconds,
    [int]$PollTimeoutSeconds
  )

  $deadline = (Get-Date).AddSeconds($PollTimeoutSeconds)
  do {
    $detail = Invoke-McpTool -Port $Port -SessionId $SessionId -ApiToken $ApiToken -Name "get_worker_run" -Arguments @{ run_id = $RunId }
    $status = $detail.status
    Write-Host "[e2e] run $RunId status: $status"
    if ($status -eq "succeeded") {
      return $detail
    }
    if (@("failed", "aborted", "abort", "timeout") -contains $status) {
      throw "Run $RunId ended with status '$status': $($detail | ConvertTo-Json -Depth 30 -Compress)"
    }
    Start-Sleep -Seconds $PollIntervalSeconds
  } while ((Get-Date) -lt $deadline)

  throw "Run $RunId did not succeed within $PollTimeoutSeconds seconds."
}

Write-Host "[e2e] base url: $BaseUrl"
if (-not $DirectWorkerRun) {
  # task-run path needs a saved task that belongs to THIS account. There is no
  # stable cross-account default slug, so require the caller to supply one
  # (e.g. via $env:CORECLAW_E2E_WORKER_TASK_ID) instead of silently using a
  # foreign slug that would 404. Find one with:
  #   curl -s -H "Authorization: Bearer $CORECLAW_API_KEY" \
  #     "$BaseUrl/api/v2/worker-tasks?offset=0&limit=1" | jq -r .data.list[0].slug
  if ([string]::IsNullOrWhiteSpace($WorkerTaskId)) {
    throw "WorkerTaskId is required for the task-run path. Set -WorkerTaskId or `$env:CORECLAW_E2E_WORKER_TASK_ID to a saved task slug owned by this account (obtain from GET /api/v2/worker-tasks), or use -DirectWorkerRun to exercise run_worker instead."
  }
  Write-Host "[e2e] worker task: $WorkerTaskId"
} else {
  Write-Host "[e2e] mode: direct worker run (run_worker), no saved task needed"
}

if (-not $SkipBuild) {
  Write-Host "[e2e] build binary"
  New-Item -ItemType Directory -Force -Path ".\dist" | Out-Null
  go build -buildvcs=false -o ".\dist\coreclaw-mcp-server-e2e.exe" .
}

$binary = ".\dist\coreclaw-mcp-server-e2e.exe"
if (-not (Test-Path $binary)) {
  throw "Expected binary at $binary. Run without -SkipBuild first."
}

Write-Host "[e2e] start local MCP HTTP server on port $Port"
$server = Start-Process -FilePath $binary -ArgumentList @("--transport", "http", "--port", "$Port", "--base-url", $BaseUrl) -PassThru -WindowStyle Hidden
try {
  Start-Sleep -Seconds 2

  $init = @{
    jsonrpc = "2.0"
    id = 1
    method = "initialize"
    params = @{
      protocolVersion = "2024-11-05"
      capabilities = @{}
      clientInfo = @{ name = "coreclaw-e2e"; version = "1.0.0" }
    }
  } | ConvertTo-Json -Depth 20 -Compress

  $initResponse = Invoke-JsonRpc -Port $Port -ApiToken $ApiToken -Body $init
  $sessionId = $initResponse.SessionId
  if ([string]::IsNullOrWhiteSpace($sessionId)) {
    throw "MCP initialize response did not include Mcp-Session-Id header."
  }
  Invoke-JsonRpc -Port $Port -SessionId $sessionId -ApiToken $ApiToken -Body '{"jsonrpc":"2.0","method":"notifications/initialized"}' | Out-Null

  Write-Host "[e2e] tools/call run_worker_task"
  $run = Invoke-McpTool -Port $Port -SessionId $sessionId -ApiToken $ApiToken -Name "run_worker_task" -Arguments @{
    worker_task_id = $WorkerTaskId
    is_async = $true
    offset = 0
    limit = 1
  }
  $runId = Get-RunId $run
  Write-Host "[e2e] created run: $runId"

  $detail = Wait-RunSucceeded -Port $Port -SessionId $sessionId -ApiToken $ApiToken -RunId $runId -PollIntervalSeconds $PollIntervalSeconds -PollTimeoutSeconds $PollTimeoutSeconds
  if ([int]$detail.results -lt 1) {
    throw "Run $runId succeeded but reported fewer than 1 result: $($detail | ConvertTo-Json -Depth 30 -Compress)"
  }
  $workerIdFromRun = $detail.scraper_slug
  if ([string]::IsNullOrWhiteSpace($workerIdFromRun)) {
    throw "Run $runId detail did not include scraper_slug/worker id: $($detail | ConvertTo-Json -Depth 30 -Compress)"
  }

  Write-Host "[e2e] tools/call list_worker_runs"
  $runList = Invoke-McpTool -Port $Port -SessionId $sessionId -ApiToken $ApiToken -Name "list_worker_runs" -Arguments @{
    offset = 0
    limit = 5
  }
  if ([int]$runList.count -lt 1 -or (-not $runList.list -or $runList.list.Count -lt 1)) {
    throw "list_worker_runs returned no runs after creating ${runId}: $($runList | ConvertTo-Json -Depth 30 -Compress)"
  }

  Write-Host "[e2e] tools/call get_last_worker_run"
  $lastRun = Invoke-McpTool -Port $Port -SessionId $sessionId -ApiToken $ApiToken -Name "get_last_worker_run"
  if ($lastRun.status -ne "succeeded") {
    throw "get_last_worker_run did not return a succeeded run after ${runId}: $($lastRun | ConvertTo-Json -Depth 30 -Compress)"
  }

  Write-Host "[e2e] tools/call get_worker"
  Invoke-McpTool -Port $Port -SessionId $sessionId -ApiToken $ApiToken -Name "get_worker" -Arguments @{ worker_id = $workerIdFromRun } | Out-Null

  Write-Host "[e2e] tools/call get_worker_input_schema"
  Invoke-McpTool -Port $Port -SessionId $sessionId -ApiToken $ApiToken -Name "get_worker_input_schema" -Arguments @{ worker_id = $workerIdFromRun } | Out-Null

  Write-Host "[e2e] tools/call get_worker_run_log"
  $log = Invoke-McpTool -Port $Port -SessionId $sessionId -ApiToken $ApiToken -Name "get_worker_run_log" -Arguments @{ run_id = $runId }
  if ([int]$log.result_count -lt 1 -and (-not $log.list -or $log.list.Count -lt 1)) {
    throw "Run $runId returned no log evidence: $($log | ConvertTo-Json -Depth 30 -Compress)"
  }

  Write-Host "[e2e] tools/call list_worker_run_results"
  $results = Invoke-McpTool -Port $Port -SessionId $sessionId -ApiToken $ApiToken -Name "list_worker_run_results" -Arguments @{
    run_id = $runId
    offset = 0
    limit = 1
  }
  if ([int]$results.count -lt 1 -or (-not $results.list -or $results.list.Count -lt 1)) {
    throw "Run $runId returned no result rows: $($results | ConvertTo-Json -Depth 30 -Compress)"
  }

  Write-Host "[e2e] tools/call export_worker_run_results"
  $export = Invoke-McpTool -Port $Port -SessionId $sessionId -ApiToken $ApiToken -Name "export_worker_run_results" -Arguments @{
    run_id = $runId
    format = "json"
  }
  if ([string]::IsNullOrWhiteSpace($export.download_url)) {
    throw "Run $runId export did not return download_url: $($export | ConvertTo-Json -Depth 30 -Compress)"
  }

  Write-Host "[e2e] tools/call get_last_worker_run_log"
  Invoke-McpTool -Port $Port -SessionId $sessionId -ApiToken $ApiToken -Name "get_last_worker_run_log" | Out-Null

  Write-Host "[e2e] tools/call list_last_worker_run_results"
  $lastResults = Invoke-McpTool -Port $Port -SessionId $sessionId -ApiToken $ApiToken -Name "list_last_worker_run_results" -Arguments @{
    offset = 0
    limit = 1
  }
  if ([int]$lastResults.count -lt 1 -or (-not $lastResults.list -or $lastResults.list.Count -lt 1)) {
    throw "list_last_worker_run_results returned no rows: $($lastResults | ConvertTo-Json -Depth 30 -Compress)"
  }

  Write-Host "[e2e] tools/call export_last_worker_run_results"
  $lastExport = Invoke-McpTool -Port $Port -SessionId $sessionId -ApiToken $ApiToken -Name "export_last_worker_run_results" -Arguments @{ format = "json" }
  if ([string]::IsNullOrWhiteSpace($lastExport.download_url)) {
    throw "export_last_worker_run_results did not return download_url: $($lastExport | ConvertTo-Json -Depth 30 -Compress)"
  }

  Write-Host "[e2e] tools/call get_worker_last_run"
  $workerLastRun = Invoke-McpTool -Port $Port -SessionId $sessionId -ApiToken $ApiToken -Name "get_worker_last_run" -Arguments @{ worker_id = $workerIdFromRun }
  if ($workerLastRun.status -ne "succeeded") {
    throw "get_worker_last_run did not return a succeeded run for ${workerIdFromRun}: $($workerLastRun | ConvertTo-Json -Depth 30 -Compress)"
  }

  Write-Host "[e2e] tools/call get_worker_last_run_log"
  Invoke-McpTool -Port $Port -SessionId $sessionId -ApiToken $ApiToken -Name "get_worker_last_run_log" -Arguments @{ worker_id = $workerIdFromRun } | Out-Null

  Write-Host "[e2e] tools/call list_worker_last_run_results"
  $workerLastResults = Invoke-McpTool -Port $Port -SessionId $sessionId -ApiToken $ApiToken -Name "list_worker_last_run_results" -Arguments @{
    worker_id = $workerIdFromRun
    offset = 0
    limit = 1
  }
  if ([int]$workerLastResults.count -lt 1 -or (-not $workerLastResults.list -or $workerLastResults.list.Count -lt 1)) {
    throw "list_worker_last_run_results returned no rows for ${workerIdFromRun}: $($workerLastResults | ConvertTo-Json -Depth 30 -Compress)"
  }

  Write-Host "[e2e] tools/call export_worker_last_run_results"
  $workerLastExport = Invoke-McpTool -Port $Port -SessionId $sessionId -ApiToken $ApiToken -Name "export_worker_last_run_results" -Arguments @{
    worker_id = $workerIdFromRun
    format = "json"
  }
  if ([string]::IsNullOrWhiteSpace($workerLastExport.download_url)) {
    throw "export_worker_last_run_results did not return download_url for ${workerIdFromRun}: $($workerLastExport | ConvertTo-Json -Depth 30 -Compress)"
  }

  if ($DirectWorkerRun) {
    Write-Host "[e2e] tools/call run_worker direct async smoke"
    $workerInput = $WorkerInputJson | ConvertFrom-Json
    $directRun = Invoke-McpTool -Port $Port -SessionId $sessionId -ApiToken $ApiToken -Name "run_worker" -Arguments @{
      worker_id = $WorkerId
      version = $WorkerVersion
      input_json = ($workerInput | ConvertTo-Json -Depth 50 -Compress)
      is_async = $true
      offset = 0
      limit = 1
    }
    $directRunId = Get-RunId $directRun
    Write-Host "[e2e] created direct worker run: $directRunId"
  }
}
finally {
  if ($server -and -not $server.HasExited) {
    Stop-Process -Id $server.Id -Force
  }
}

Write-Host "[e2e] completed"
