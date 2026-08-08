param(
  [string]$BaseUrl = $(if ($env:CORECLAW_BASE_URL) { $env:CORECLAW_BASE_URL } else { "https://openapi.coreclaw.com" }),
  [string]$ApiToken = $env:CORECLAW_API_KEY,
  [int]$Port = 3300,
  [switch]$SkipRun
)

$ErrorActionPreference = "Stop"
Add-Type -AssemblyName System.Net.Http

function Invoke-CoreClaw {
  param(
    [string]$Method,
    [string]$Path,
    [hashtable]$Query = @{},
    [object]$Body = $null,
    [bool]$Auth = $false
  )

  $builder = [System.UriBuilder]::new(($BaseUrl.TrimEnd("/") + $Path))
  if ($Query.Count -gt 0) {
    $pairs = foreach ($key in $Query.Keys) {
      if ($null -ne $Query[$key] -and "$($Query[$key])" -ne "") {
        [System.Uri]::EscapeDataString($key) + "=" + [System.Uri]::EscapeDataString("$($Query[$key])")
      }
    }
    $builder.Query = ($pairs -join "&")
  }

  $headers = @{}
  if ($Auth) {
    if ([string]::IsNullOrWhiteSpace($ApiToken)) {
      throw "CORECLAW_API_KEY is required for authenticated verification."
    }
    $headers["Authorization"] = "Bearer $ApiToken"
  }

  $args = @{
    Method = $Method
    Uri = $builder.Uri.AbsoluteUri
    Headers = $headers
  }
  if ($null -ne $Body) {
    $args.ContentType = "application/json"
    $args.Body = ($Body | ConvertTo-Json -Depth 50 -Compress)
  }

  $response = Invoke-RestMethod @args
  if ($response.code -ne 0) {
    throw "CoreClaw returned code=$($response.code) message=$($response.message) for $Method $Path"
  }
  return $response
}

function Assert-ToolCount {
  param([string]$Text)
  $json = $Text | ConvertFrom-Json
  $tools = $json.result.tools
  if ($tools.Count -ne 42) {
    throw "Expected 42 MCP tools, got $($tools.Count)"
  }
  $excluded = @("get_worker_internal", "create_worker_version", "update_worker_version", "list_queued_worker_runs")
  foreach ($name in $excluded) {
    if ($tools.name -contains $name) {
      throw "Excluded tool was exposed: $name"
    }
  }
}

function Invoke-JsonRpc {
  param(
    [int]$Port,
    [string]$Body,
    [string]$SessionId = ""
  )

  $handler = [System.Net.Http.HttpClientHandler]::new()
  $client = [System.Net.Http.HttpClient]::new($handler)
  try {
    $request = [System.Net.Http.HttpRequestMessage]::new([System.Net.Http.HttpMethod]::Post, "http://127.0.0.1:$Port/mcp")
    $request.Content = [System.Net.Http.StringContent]::new($Body, [System.Text.Encoding]::UTF8, "application/json")
    if (-not [string]::IsNullOrWhiteSpace($SessionId)) {
      $request.Headers.TryAddWithoutValidation("Mcp-Session-Id", $SessionId) | Out-Null
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

Write-Host "[verify] base url: $BaseUrl"

Write-Host "[verify] public GET endpoints"
Invoke-CoreClaw GET "/api/v2/proxy/region" @{ language = "en" } | Out-Null
$store = Invoke-CoreClaw GET "/api/v2/store" @{ offset = 1; limit = 5; keyword = "" }

if (-not [string]::IsNullOrWhiteSpace($ApiToken)) {
  Write-Host "[verify] authenticated GET endpoints"
  Invoke-CoreClaw GET "/api/v2/users/account" @{} $null $true | Out-Null
  Invoke-CoreClaw GET "/api/v2/worker-runs" @{ offset = 1; limit = 5 } $null $true | Out-Null
  Invoke-CoreClaw GET "/api/v2/worker-runs/last" @{} $null $true | Out-Null
  Invoke-CoreClaw GET "/api/v2/worker-tasks" @{ offset = 1; limit = 5 } $null $true | Out-Null
  Invoke-CoreClaw GET "/api/v2/workers" @{ offset = 1; limit = 5 } $null $true | Out-Null
} else {
  Write-Host "[verify] CORECLAW_API_KEY not set; skipping authenticated upstream checks"
}

Write-Host "[verify] build binary"
New-Item -ItemType Directory -Force -Path ".\dist" | Out-Null
go build -buildvcs=false -o ".\dist\coreclaw-mcp-server-windows-amd64.exe" .

Write-Host "[verify] start local HTTP server"
$server = Start-Process -FilePath ".\dist\coreclaw-mcp-server-windows-amd64.exe" -ArgumentList @("--transport", "http", "--port", "$Port", "--base-url", $BaseUrl) -PassThru -WindowStyle Hidden
try {
  Start-Sleep -Seconds 2
  $init = '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"verify","version":"1.0.0"}}}'
  $initResponse = Invoke-JsonRpc -Port $Port -Body $init
  $sessionId = $initResponse.SessionId
  if ([string]::IsNullOrWhiteSpace($sessionId)) {
    throw "MCP initialize response did not include Mcp-Session-Id header"
  }
  Invoke-JsonRpc -Port $Port -SessionId $sessionId -Body '{"jsonrpc":"2.0","method":"notifications/initialized"}' | Out-Null
  $listText = (Invoke-JsonRpc -Port $Port -SessionId $sessionId -Body '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}').Body
  Assert-ToolCount $listText

  Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:$Port/mcp/list_proxy_regions" -ContentType "application/json" -Body '{"language":"en"}' | Out-Null
  Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:$Port/mcp/list_store_workers" -ContentType "application/json" -Body '{"offset":1,"limit":2}' | Out-Null

  # 1-based pagination check: offset=1 returns page 1, offset=2 returns page 2,
  # and the two pages must differ (not the same rows). Out-of-range offset
  # returns an empty list.
  $page1 = (Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:$Port/mcp/list_store_workers" -ContentType "application/json" -Body '{"offset":1,"limit":20}').scraper
  $page2 = (Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:$Port/mcp/list_store_workers" -ContentType "application/json" -Body '{"offset":2,"limit":20}').scraper
  if ($page1.Count -eq 0) {
    throw "list_store_workers offset=1 returned 0 rows; store endpoint has no data to verify pagination."
  }
  if ($page1[0].slug -eq $page2[0].slug) {
    throw "list_store_workers offset=1 and offset=2 returned the same first slug ($($page1[0].slug)); 1-based pagination is broken."
  }
  $page1GroundTruth = (Invoke-RestMethod -Method Get -Uri "$BaseUrl/api/v2/store?offset=1&limit=20" -Headers @{ "Authorization" = "Bearer $ApiToken" }).data.scraper
  if ($page1[0].slug -ne $page1GroundTruth[0].slug) {
    throw "list_store_workers offset=1 first slug $($page1[0].slug) != ground-truth $($page1GroundTruth[0].slug)"
  }
  Write-Host "[verify] list_store_workers 1-based pagination OK (page1=$($page1[0].slug), page2=$($page2[0].slug))"

  if (-not [string]::IsNullOrWhiteSpace($ApiToken)) {
    Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:$Port/mcp/get_account_info" -Headers @{ "api-key" = $ApiToken } -ContentType "application/json" -Body '{}' | Out-Null
  }
}
finally {
  if ($server -and -not $server.HasExited) {
    Stop-Process -Id $server.Id -Force
  }
}

Write-Host "[verify] completed"
