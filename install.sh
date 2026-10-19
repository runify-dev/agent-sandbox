#!/usr/bin/env bash
# Agent Sandbox 一键安装 / 升级脚本（Linux，需要 root）
#
#   curl -fsSL https://raw.githubusercontent.com/runify-dev/agent-sandbox/main/install.sh | sudo bash
#
# 可用环境变量覆盖默认值（sudo 需要用 sudo -E 或写在 sudo 后面，如 sudo VERSION=0.1.0 bash）：
#   INSTALL_DIR   安装目录，默认 /opt/agent-sandbox
#   DATA_DIR      沙箱数据目录，默认 $INSTALL_DIR/data
#   VERSION       镜像版本，默认 latest
#   SANDBOX_BIND  API 监听地址，默认 127.0.0.1（只允许本机访问）
#   SANDBOX_PORT  API 端口，默认 8090
#   MIRROR=aliyun 安装 Docker 时使用阿里云镜像源（国内服务器）
#   AGENT_SANDBOX_SKIP_PULL=1  不拉取镜像（离线安装：先用 docker load 导入镜像）
#   AGENT_SANDBOX_IMAGE_PREFIX 镜像仓库前缀，默认 ghcr.io/runify-dev/，可改为自建的镜像仓库
#
# 脚本只需要下载这一个文件，compose 配置内嵌在下面的 write_compose 中。
#
# 重复执行即为升级：保留已有的 .env（包括 API key），拉取新镜像并重启服务。
set -euo pipefail

REPO_RAW="https://raw.githubusercontent.com/runify-dev/agent-sandbox/main"
INSTALL_DIR="${INSTALL_DIR:-/opt/agent-sandbox}"
DATA_DIR="${DATA_DIR:-$INSTALL_DIR/data}"
VERSION="${VERSION:-latest}"
SANDBOX_BIND="${SANDBOX_BIND:-127.0.0.1}"
SANDBOX_PORT="${SANDBOX_PORT:-8090}"

log() { printf '\033[1;32m==>\033[0m %s\n' "$*"; }
die() { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }

if [ "$(id -u)" -ne 0 ] && [ -z "${AGENT_SANDBOX_ALLOW_NONROOT:-}" ]; then
  die "请使用 root 运行，例如：curl -fsSL $REPO_RAW/install.sh | sudo bash"
fi

# 1. Docker
if ! command -v docker >/dev/null 2>&1; then
  log "未检测到 Docker，开始安装"
  if [ "${MIRROR:-}" = "aliyun" ]; then
    curl -fsSL --retry 3 https://get.docker.com | sh -s -- --mirror Aliyun
  else
    curl -fsSL --retry 3 https://get.docker.com | sh
  fi
fi
if command -v systemctl >/dev/null 2>&1; then
  systemctl enable --now docker >/dev/null 2>&1 || true
fi
docker info >/dev/null 2>&1 || die "Docker 未运行"
docker compose version >/dev/null 2>&1 || die "需要 Docker Compose v2（docker compose 命令）"

# 2. 目录与 compose 文件（每次安装 / 升级都覆盖，配置都在 .env 里）
log "安装目录 $INSTALL_DIR，数据目录 $DATA_DIR"
mkdir -p "$INSTALL_DIR" "$DATA_DIR"
write_compose() {
  cat <<'COMPOSE'
# 由 install.sh 生成，升级时会被覆盖；修改配置请编辑同目录的 .env
# 运行时镜像较大，服务第一次启动时如果本地没有会自动拉取，期间 /healthz 不可用
services:
  sandbox-server:
    image: ${AGENT_SANDBOX_IMAGE_PREFIX-ghcr.io/runify-dev/}agent-sandbox-server:${AGENT_SANDBOX_VERSION:-latest}
    restart: unless-stopped
    environment:
      SANDBOX_API_KEY: ${SANDBOX_API_KEY:?SANDBOX_API_KEY is required}
      SANDBOX_IMAGE: ${AGENT_SANDBOX_IMAGE_PREFIX-ghcr.io/runify-dev/}agent-sandbox-runtime:${AGENT_SANDBOX_VERSION:-latest}
      SANDBOX_DATA_ROOT: ${SANDBOX_DATA_ROOT:?SANDBOX_DATA_ROOT is required}
      SANDBOX_NETWORK: ${SANDBOX_NETWORK:-proxy}
      SANDBOX_EGRESS_ALLOWLIST: ${SANDBOX_EGRESS_ALLOWLIST:-pypi.org,files.pythonhosted.org,registry.npmjs.org}
      SANDBOX_EGRESS_PRIVATE_CIDRS: ${SANDBOX_EGRESS_PRIVATE_CIDRS:-}
      SANDBOX_PIP_INDEX_URL: ${SANDBOX_PIP_INDEX_URL:-}
      SANDBOX_NPM_REGISTRY: ${SANDBOX_NPM_REGISTRY:-}
      SANDBOX_RUNTIME: ${SANDBOX_RUNTIME:-}
      SANDBOX_MEMORY_MB: ${SANDBOX_MEMORY_MB:-1024}
      SANDBOX_CPUS: ${SANDBOX_CPUS:-1.0}
      SANDBOX_MAX_SESSIONS: ${SANDBOX_MAX_SESSIONS:-100}
      SANDBOX_MAX_TIMEOUT: ${SANDBOX_MAX_TIMEOUT:-600}
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      # 挂载源路径由宿主机上的 dockerd 解析，所以服务容器内外的路径必须一致
      - ${SANDBOX_DATA_ROOT}:${SANDBOX_DATA_ROOT}
    networks:
      default: {}
      egress:
        aliases: [sandbox-egress-proxy]
    ports:
      - "${SANDBOX_BIND:-127.0.0.1}:${SANDBOX_PORT:-8090}:8090"
    logging:
      driver: json-file
      options:
        max-size: 50m
        max-file: "3"

networks:
  egress:
    name: agent-sandbox-egress
    internal: true
COMPOSE
}
write_compose >"$INSTALL_DIR/docker-compose.yml"

# 3. 配置：已有 .env 时只更新版本，其余保持不变
ENV_FILE="$INSTALL_DIR/.env"
if [ -f "$ENV_FILE" ]; then
  log "保留已有配置 $ENV_FILE"
  if grep -q '^AGENT_SANDBOX_VERSION=' "$ENV_FILE"; then
    sed -i.bak "s/^AGENT_SANDBOX_VERSION=.*/AGENT_SANDBOX_VERSION=$VERSION/" "$ENV_FILE" && rm -f "$ENV_FILE.bak"
  else
    echo "AGENT_SANDBOX_VERSION=$VERSION" >>"$ENV_FILE"
  fi
else
  log "生成配置 $ENV_FILE"
  umask 077
  cat >"$ENV_FILE" <<EOF
SANDBOX_API_KEY=$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')
SANDBOX_DATA_ROOT=$DATA_DIR
AGENT_SANDBOX_VERSION=$VERSION
SANDBOX_BIND=$SANDBOX_BIND
SANDBOX_PORT=$SANDBOX_PORT
# 白名单代理允许访问的域名，* 表示所有公网
SANDBOX_EGRESS_ALLOWLIST=pypi.org,files.pythonhosted.org,registry.npmjs.org
EOF
fi
chmod 600 "$ENV_FILE"

# 4. 拉取镜像并启动
cd "$INSTALL_DIR"
set -a
# shellcheck source=/dev/null
. "$ENV_FILE"
set +a
RUNTIME_IMAGE="${AGENT_SANDBOX_IMAGE_PREFIX-ghcr.io/runify-dev/}agent-sandbox-runtime:${AGENT_SANDBOX_VERSION}"
if [ -z "${AGENT_SANDBOX_SKIP_PULL:-}" ]; then
  log "拉取镜像（运行时镜像约 3GB，首次需要一些时间）"
  docker compose pull
  docker pull "$RUNTIME_IMAGE"
fi
log "启动服务"
docker compose up -d

# 5. 等待就绪
URL="http://127.0.0.1:${SANDBOX_PORT:-8090}/healthz"
for _ in $(seq 1 60); do
  if curl -sf "$URL" >/dev/null 2>&1; then
    log "安装完成"
    cat <<EOF

  API 地址:  http://${SANDBOX_BIND:-127.0.0.1}:${SANDBOX_PORT:-8090}
  API Key:   见 $ENV_FILE 中的 SANDBOX_API_KEY
  数据目录:  $DATA_DIR

  查看日志:  cd $INSTALL_DIR && docker compose logs -f
  修改配置:  编辑 $ENV_FILE 后执行 cd $INSTALL_DIR && docker compose up -d
  升级:      重新执行安装命令（可加 VERSION=x.y.z）
  卸载:      cd $INSTALL_DIR && docker compose down（数据目录需手动删除）

EOF
    exit 0
  fi
  sleep 2
done
docker compose logs --tail 50
die "服务未能在 2 分钟内就绪，请检查上面的日志"
