#!/usr/bin/env bash
# Direct CoreClaw OpenAPI v2 smoke/debug helper. This bypasses MCP and calls
# the upstream API with the same endpoint shapes used by the MCP tools.
#
# Usage:
#   export CORECLAW_API_KEY='<your token>'
#   ./scripts/debug-scraper.sh [worker_id] [input_json]
set -euo pipefail

BASE="${CORECLAW_BASE_URL:-https://openapi.coreclaw.com}"
KEY="${CORECLAW_API_KEY:?export CORECLAW_API_KEY first}"
WORKER_ID="${1:-}"
INPUT_JSON="${2:-{\"keyword\":\"coffee\",\"limit\":10}}"

pretty() { python3 -m json.tool 2>/dev/null || cat; }
banner() { printf "\n==> %s\n" "$*"; }

auth=(-H "Authorization: Bearer ${KEY}")

banner "account"
curl -sS "${BASE}/api/v2/users/account" "${auth[@]}" | pretty

banner "store workers"
curl -sS -G "${BASE}/api/v2/store" --data-urlencode "offset=0" --data-urlencode "limit=5" | pretty

if [[ -z "${WORKER_ID}" ]]; then
  banner "worker-specific checks skipped"
  echo "Pass a worker_id or owner~worker path as the first argument to test schema/run endpoints."
  exit 0
fi

escaped_worker_id="$(python3 - <<PY
import urllib.parse
print(urllib.parse.quote("$WORKER_ID", safe="~"))
PY
)"

banner "worker detail"
curl -sS "${BASE}/api/v2/workers/${escaped_worker_id}" "${auth[@]}" | pretty

banner "worker input schema"
curl -sS "${BASE}/api/v2/workers/${escaped_worker_id}/input-schema" | pretty

banner "run worker synchronously"
body="$(cat <<JSON
{
  "version": "latest",
  "input": ${INPUT_JSON},
  "is_async": false,
  "offset": 0,
  "limit": 20
}
JSON
)"
curl -sS -X POST "${BASE}/api/v2/workers/${escaped_worker_id}/runs" \
  "${auth[@]}" \
  -H "Content-Type: application/json" \
  -d "${body}" | pretty
