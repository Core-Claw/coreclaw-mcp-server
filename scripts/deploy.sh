#!/usr/bin/env bash
# 自动部署 coreclaw-mcp-server 到 Ubuntu 服务器。
# 从项目根目录的 .env 读取配置，支持用 --dir 覆盖部署目录。
#
# 用法：
#   ./scripts/deploy.sh                 # 完整部署
#   ./scripts/deploy.sh --dir /opt/xxx  # 覆盖部署目录
#   ./scripts/deploy.sh --skip-build    # 跳过编译（复用 dist/ 下二进制）
#   ./scripts/deploy.sh --no-systemd    # 不写 systemd，只上传二进制
#   ./scripts/deploy.sh --check         # 部署后跑 initialize 健康检查

set -euo pipefail

# --- 定位项目根目录 ---
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${ROOT_DIR}"

# --- 加载 .env ---
if [[ ! -f .env ]]; then
  echo "[deploy] .env 不存在，请先 cp .env.example .env 并填写真实值" >&2
  exit 1
fi
set -o allexport
# shellcheck disable=SC1091
source .env
set +o allexport

# --- 默认值 ---
SSH_PORT="${SSH_PORT:-22}"
SSH_USER="${SSH_USER:-ubuntu}"
MCP_PORT="${MCP_PORT:-3000}"
CORECLAW_BASE_URL="${CORECLAW_BASE_URL:-https://openapi.coreclaw.com}"
SERVICE_NAME="${SERVICE_NAME:-coreclaw-mcp-server}"
OPEN_UFW="${OPEN_UFW:-no}"
USE_SYSTEMD="${USE_SYSTEMD:-yes}"
USE_SUDO="${USE_SUDO:-yes}"
SERVICE_USER="${SERVICE_USER:-root}"

# --- 解析命令行 ---
SKIP_BUILD=no
DO_CHECK=no
while [[ $# -gt 0 ]]; do
  case "$1" in
    --dir)         DEPLOY_DIR="$2"; shift 2 ;;
    --skip-build)  SKIP_BUILD=yes; shift ;;
    --no-systemd)  USE_SYSTEMD=no; shift ;;
    --check)       DO_CHECK=yes; shift ;;
    -h|--help)
      sed -n '2,12p' "$0"; exit 0 ;;
    *) echo "[deploy] 未知参数: $1" >&2; exit 1 ;;
  esac
done

# --- 校验必需参数 ---
: "${SSH_HOST:?SSH_HOST 未设置}"
: "${DEPLOY_DIR:?DEPLOY_DIR 未设置（可用 --dir 传入）}"
# 远程 HTTP 部署下不再需要服务端共享 key：api-key 是每个用户自己的凭证，
# 由 MCP 客户端通过 HTTP 请求头传入。CORECLAW_API_KEY 保留为可选（留空即可）。

BINARY_NAME="coreclaw-mcp-server"
LOCAL_BIN="dist/${BINARY_NAME}-linux-amd64"

# --- 组装 ssh/scp 参数 ---
SSH_OPTS=(-p "${SSH_PORT}" -o StrictHostKeyChecking=accept-new)
SCP_OPTS=(-P "${SSH_PORT}" -o StrictHostKeyChecking=accept-new)
if [[ -n "${SSH_KEY:-}" ]]; then
  KEY_PATH="${SSH_KEY/#\~/$HOME}"
  SSH_OPTS+=(-i "${KEY_PATH}")
  SCP_OPTS+=(-i "${KEY_PATH}")
fi
REMOTE="${SSH_USER}@${SSH_HOST}"

# 所有远端操作都通过 sudo 以 root 身份执行（SSH 登录仍是普通用户）。
# 需要 SSH_USER 配置免密 sudo，否则 ssh 非交互环境下会卡住。
REMOTE_SUDO="sudo -n"
[[ "${USE_SUDO}" = "no" ]] && REMOTE_SUDO=""

echo "[deploy] target : ${REMOTE}:${DEPLOY_DIR} (run-as=${SERVICE_USER})"
echo "[deploy] service: ${SERVICE_NAME} (systemd=${USE_SYSTEMD}, sudo=${USE_SUDO})"
echo "[deploy] runtime: --port ${MCP_PORT} --base-url ${CORECLAW_BASE_URL}"

# 预检查：确认 ${SSH_USER} 在远端可以免密 sudo
if [[ -n "${REMOTE_SUDO}" ]]; then
  if ! ssh "${SSH_OPTS[@]}" "${REMOTE}" "${REMOTE_SUDO} true" >/dev/null 2>&1; then
    echo "[deploy] ${SSH_USER}@${SSH_HOST} 无法免密 sudo。请在远端执行：" >&2
    echo "  echo '${SSH_USER} ALL=(ALL) NOPASSWD:ALL' | sudo tee /etc/sudoers.d/${SSH_USER}" >&2
    echo "或在 .env 里把 USE_SUDO 设为 no（此时需 SSH_USER 对 DEPLOY_DIR 有写权限，且无法写 systemd/ufw）" >&2
    exit 1
  fi
fi

# --- 1. 本地交叉编译 ---
if [[ "${SKIP_BUILD}" = "no" ]]; then
  echo "[deploy] 编译 linux/amd64 二进制 -> ${LOCAL_BIN}"
  mkdir -p dist
  GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
    go build -ldflags "-s -w" -o "${LOCAL_BIN}" .
fi

if [[ ! -f "${LOCAL_BIN}" ]]; then
  echo "[deploy] 找不到 ${LOCAL_BIN}（--skip-build 时需要先构建一次）" >&2
  exit 1
fi

# --- 2. 上传二进制：scp 不能 sudo，先进 /tmp 再由 root 搬到目标目录 ---
echo "[deploy] 上传二进制到远端（经 /tmp 中转）"
TMP_PATH="/tmp/${BINARY_NAME}.$$.$(date +%s)"
scp "${SCP_OPTS[@]}" "${LOCAL_BIN}" "${REMOTE}:${TMP_PATH}"
ssh "${SSH_OPTS[@]}" "${REMOTE}" "
  set -e
  ${REMOTE_SUDO} mkdir -p '${DEPLOY_DIR}'
  ${REMOTE_SUDO} mv '${TMP_PATH}' '${DEPLOY_DIR}/${BINARY_NAME}'
  ${REMOTE_SUDO} chown ${SERVICE_USER}:${SERVICE_USER} '${DEPLOY_DIR}/${BINARY_NAME}'
  ${REMOTE_SUDO} chmod 0755 '${DEPLOY_DIR}/${BINARY_NAME}'
"

# --- 3. 可选：放通 ufw 端口 ---
if [[ "${OPEN_UFW}" = "yes" ]]; then
  echo "[deploy] ufw allow ${MCP_PORT}/tcp"
  ssh "${SSH_OPTS[@]}" "${REMOTE}" "${REMOTE_SUDO} ufw allow ${MCP_PORT}/tcp || true"
fi

# --- 4. 写入 systemd 并重启 ---
if [[ "${USE_SYSTEMD}" = "yes" ]]; then
  echo "[deploy] 写入 systemd 单元并重启服务（运行身份：${SERVICE_USER}）"
  UNIT_PATH="/etc/systemd/system/${SERVICE_NAME}.service"
  ssh "${SSH_OPTS[@]}" "${REMOTE}" "${REMOTE_SUDO} tee ${UNIT_PATH} >/dev/null" <<EOF
[Unit]
Description=CoreClaw MCP Server
After=network.target

[Service]
User=${SERVICE_USER}
WorkingDirectory=${DEPLOY_DIR}
ExecStart=${DEPLOY_DIR}/${BINARY_NAME} --transport http --port ${MCP_PORT} --base-url ${CORECLAW_BASE_URL}
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF

  ssh "${SSH_OPTS[@]}" "${REMOTE}" "
    set -e
    ${REMOTE_SUDO} systemctl daemon-reload
    ${REMOTE_SUDO} systemctl enable ${SERVICE_NAME}
    ${REMOTE_SUDO} systemctl restart ${SERVICE_NAME}
    ${REMOTE_SUDO} systemctl --no-pager --full status ${SERVICE_NAME} | head -n 15
  "
else
  echo "[deploy] 跳过 systemd，二进制已就位：${DEPLOY_DIR}/${BINARY_NAME}"
fi

# --- 5. 可选：本地健康检查 ---
if [[ "${DO_CHECK}" = "yes" ]]; then
  echo "[deploy] 健康检查 http://${SSH_HOST}:${MCP_PORT}/mcp"
  sleep 1
  curl -sS -X POST "http://${SSH_HOST}:${MCP_PORT}/mcp" \
    -H "Content-Type: application/json" \
    -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"deploy-check","version":"1.0"}}}' \
    | head -c 400
  echo
fi

echo "[deploy] 完成"
