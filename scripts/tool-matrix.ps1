$ErrorActionPreference = "Stop"

$endpointCsv = Join-Path (Split-Path $PSScriptRoot -Parent) "..\exported-api-docs\endpoints.csv"
if (-not (Test-Path $endpointCsv)) {
  $endpointCsv = "D:\Coreclaw_Work\github\exported-api-docs\endpoints.csv"
}

$excluded = @(
  "POST /api/v2/workers/{workerId}/versions",
  "PUT /api/v2/workers/{workerId}/versions/{version}",
  "GET /api/v2/workers/{workerId}/internal"
)

Import-Csv $endpointCsv |
  Where-Object { $excluded -notcontains ("$($_.method) $($_.path)") } |
  Select-Object tag, method, path, operation_id, summary |
  Format-Table -AutoSize
