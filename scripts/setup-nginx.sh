#!/usr/bin/env bash
# 把 deploy/nginx/coreclaw-mcp-server.conf 部署到远端 Ubuntu：
#   /etc/nginx/sites-available/<SERVICE_NAME>.conf
#   /etc/nginx/sites-enabled/<SERVICE_NAME>.conf  (软链)
# 然后 nginx -t && systemctl reload nginx。
#
# 从项目根 .env 读配置；命令行参数允许覆盖：
#   ./scripts/setup-nginx.sh                   # 默认行为
#   ./scripts/setup-nginx.sh --server mcp.x.io
#   ./scripts/setup-nginx.sh --listen 8080
#   ./scripts/setup-nginx.sh --dry-run         # 只打印生成的 conf

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${ROOT_DIR}"

TEMPLATE="deploy/nginx/mcp.coreclaw.com.conf"
[[ -f "${TEMPLATE}" ]] || { echo "[nginx] 模板不存在：${TEMPLATE}" >&2; exit 1; }

if [[ ! -f .env ]]; then
  echo "[nginx] .env 不存在，请先 cp .env.example .env 并填写真实值" >&2
  exit 1
fi
set -o allexport
# shellcheck disable=SC1091
source .env
set +o allexport

# --- 默认值 ---
SSH_PORT="${SSH_PORT:-22}"
SSH_USER="${SSH_USER:-ubuntu}"
SERVICE_NAME="${SERVICE_NAME:-coreclaw-mcp-server}"
MCP_PORT="${MCP_PORT:-3000}"
USE_SUDO="${USE_SUDO:-yes}"
NGINX_LISTEN_PORT="${NGINX_LISTEN_PORT:-80}"
NGINX_SERVER_NAME="${NGINX_SERVER_NAME:-_}"

DRY_RUN=no
while [[ $# -gt 0 ]]; do
  case "$1" in
    --server)   NGINX_SERVER_NAME="$2"; shift 2 ;;
    --listen)   NGINX_LISTEN_PORT="$2"; shift 2 ;;
    --dry-run)  DRY_RUN=yes; shift ;;
    -h|--help)  sed -n '2,12p' "$0"; exit 0 ;;
    *) echo "[nginx] 未知参数: $1" >&2; exit 1 ;;
  esac
done

: "${SSH_HOST:?SSH_HOST 未设置}"

# 部署到远端的 conf 文件名：默认用 server_name（例 mcp.coreclaw.com.conf）。
# 若是 _/空/含非法字符（空格、斜杠），回退到 SERVICE_NAME，避免奇怪的文件路径。
if [[ -z "${NGINX_SERVER_NAME}" || "${NGINX_SERVER_NAME}" = "_" \
      || "${NGINX_SERVER_NAME}" == *[\ /]* ]]; then
  CONF_BASENAME="${SERVICE_NAME}"
else
  CONF_BASENAME="${NGINX_SERVER_NAME}"
fi

REMOTE_SUDO="sudo -n"
[[ "${USE_SUDO}" = "no" ]] && REMOTE_SUDO=""

# --- 生成最终 conf（sed 替换占位符）---
GENERATED="$(mktemp -t ${CONF_BASENAME}.conf.XXXXXX)"
trap 'rm -f "${GENERATED}"' EXIT
sed \
  -e "s|__SERVICE_NAME__|${SERVICE_NAME}|g" \
  -e "s|__NGINX_SERVER_NAME__|${NGINX_SERVER_NAME}|g" \
  -e "s|__NGINX_LISTEN_PORT__|${NGINX_LISTEN_PORT}|g" \
  -e "s|__MCP_PORT__|${MCP_PORT}|g" \
  "${TEMPLATE}" > "${GENERATED}"

echo "[nginx] target       : ${SSH_USER}@${SSH_HOST}:${SSH_PORT}"
echo "[nginx] server_name  : ${NGINX_SERVER_NAME}"
echo "[nginx] listen       : ${NGINX_LISTEN_PORT} -> 127.0.0.1:${MCP_PORT}"
echo "[nginx] conf file    : /etc/nginx/sites-available/${CONF_BASENAME}.conf"

if [[ "${DRY_RUN}" = "yes" ]]; then
  echo "----- 生成的 conf -----"
  cat "${GENERATED}"
  exit 0
fi

# --- SSH / SCP 参数 ---
SSH_OPTS=(-p "${SSH_PORT}" -o StrictHostKeyChecking=accept-new)
SCP_OPTS=(-P "${SSH_PORT}" -o StrictHostKeyChecking=accept-new)
if [[ -n "${SSH_KEY:-}" ]]; then
  KEY_PATH="${SSH_KEY/#\~/$HOME}"
  SSH_OPTS+=(-i "${KEY_PATH}")
  SCP_OPTS+=(-i "${KEY_PATH}")
fi
REMOTE="${SSH_USER}@${SSH_HOST}"

# 免密 sudo 预检
if [[ -n "${REMOTE_SUDO}" ]]; then
  if ! ssh "${SSH_OPTS[@]}" "${REMOTE}" "${REMOTE_SUDO} true" >/dev/null 2>&1; then
    echo "[nginx] ${SSH_USER}@${SSH_HOST} 无法免密 sudo，参考 deploy.sh 错误提示" >&2
    exit 1
  fi
fi

# 远端 nginx 现状自检：先于部署暴露既有坏配置（悬空软链 / 语法错），
# 避免我们的部署结果被误判成故障根因。
if ! ssh "${SSH_OPTS[@]}" "${REMOTE}" "${REMOTE_SUDO} nginx -t" >/dev/null 2>&1; then
  echo "[nginx] 远端 nginx 当前已无法通过 nginx -t（部署前就存在的历史问题），错误详情：" >&2
  ssh "${SSH_OPTS[@]}" "${REMOTE}" "${REMOTE_SUDO} nginx -t" >&2 || true
  echo >&2
  echo "[nginx] 常见原因：sites-enabled 里有指向已删除文件的悬空软链。修复建议：" >&2
  echo "  ssh -p ${SSH_PORT} ${REMOTE} \"${REMOTE_SUDO} find /etc/nginx/sites-enabled -xtype l -print\"" >&2
  echo "  # 确认无误后再清理：" >&2
  echo "  ssh -p ${SSH_PORT} ${REMOTE} \"${REMOTE_SUDO} find /etc/nginx/sites-enabled -xtype l -delete && ${REMOTE_SUDO} nginx -t\"" >&2
  exit 1
fi

# 远端若没装 nginx，提前给出明确提示
ssh "${SSH_OPTS[@]}" "${REMOTE}" "command -v nginx >/dev/null" || {
  echo "[nginx] 远端未安装 nginx，请先执行：sudo apt update && sudo apt install -y nginx" >&2
  exit 1
}

# --- 上传到 /tmp，再由 root 搬到 sites-available ---
REMOTE_TMP="/tmp/${CONF_BASENAME}.conf.$$"
AVAILABLE="/etc/nginx/sites-available/${CONF_BASENAME}.conf"
ENABLED="/etc/nginx/sites-enabled/${CONF_BASENAME}.conf"

scp "${SCP_OPTS[@]}" "${GENERATED}" "${REMOTE}:${REMOTE_TMP}"

ssh "${SSH_OPTS[@]}" "${REMOTE}" "
  set -e
  ${REMOTE_SUDO} mkdir -p /etc/nginx/sites-available /etc/nginx/sites-enabled
  ${REMOTE_SUDO} mv '${REMOTE_TMP}' '${AVAILABLE}'
  ${REMOTE_SUDO} chown root:root '${AVAILABLE}'
  ${REMOTE_SUDO} chmod 0644 '${AVAILABLE}'
  ${REMOTE_SUDO} ln -sf '${AVAILABLE}' '${ENABLED}'

  # 先做语法校验，失败就回滚禁用软链，避免打崩已在跑的 nginx
  if ! ${REMOTE_SUDO} nginx -t; then
    echo '[nginx] nginx -t 失败，回滚 sites-enabled 软链' >&2
    ${REMOTE_SUDO} rm -f '${ENABLED}'
    exit 1
  fi

  ${REMOTE_SUDO} systemctl reload nginx
  ${REMOTE_SUDO} systemctl --no-pager --full status nginx | head -n 10
"

echo "[nginx] 完成。可测试："
if [[ "${NGINX_SERVER_NAME}" = "_" ]]; then
  echo "  curl -s -X POST http://${SSH_HOST}:${NGINX_LISTEN_PORT}/mcp -H 'Content-Type: application/json' -d '{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"2024-11-05\",\"capabilities\":{},\"clientInfo\":{\"name\":\"t\",\"version\":\"1\"}}}'"
else
  echo "  curl -s -X POST http://${NGINX_SERVER_NAME}:${NGINX_LISTEN_PORT}/mcp -H 'Content-Type: application/json' -d '{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"2024-11-05\",\"capabilities\":{},\"clientInfo\":{\"name\":\"t\",\"version\":\"1\"}}}'"
fi
