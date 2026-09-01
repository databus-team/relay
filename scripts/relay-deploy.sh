#!/usr/bin/env bash
#
# relay 一键部署助手
#
# 两条腿:
#   远端(exec)  —— 自动:经中转 `relay push`/`relay exec` 把新二进制下发到远端执行器;
#                    自动探测执行器 OS/arch 并交叉编译对应版本。
#                    RESTART=1 时自动 detached 换装并重启(先停->换->起,且不能砍
#                    掉正在服务本连接的 relay,故延迟后分离执行)。
#   中转(transit) —— 半自动:中转无任何程序化通道(SSH 不可达、非 executor、relay 只
#                  做文件交换),只能经 code-server web 人工上传替换。脚本构建 linux
#                  二进制并打印精确手工步骤。
#
# 用法:
#   RESTART=1 make deploy-remote    # 部署远端 + 自动替换重启
#   make deploy-remote              # 只下发,不重启(安全)
#   make deploy-transit             # 只出中转手工清单
#   make deploy                     # 全跑
#
# 依赖:`relay` CLI 在 PATH;配置为共享 config;远端用同一 config 以 executor 跑 relay watch。
set -euo pipefail

CONFIG="${RELAY_CONFIG:-$HOME/.relay/config.yaml}"
WATCH="${RELAY_WATCH:-}"
RESTART="${RESTART:-0}"

# 版本 stamp:与 Makefile 一致,保证产出的远端/中转二进制带同一标识,relay version 才能正确对比。
GIT_COMMIT="$(git rev-parse --short HEAD 2>/dev/null || true)"
GIT_VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
STAMP="$(printf '%s%s%s' \
  "-X github.com/user/relay/internal/version.Version=$GIT_VERSION " \
  "-X github.com/user/relay/internal/version.Commit=$GIT_COMMIT " \
  "-X github.com/user/relay/internal/version.Date=$(date -u +%Y-%m-%dT%H:%M:%SZ)")"

log()  { printf '\033[1;34m[deploy]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[deploy]\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31m[deploy]\033[0m %s\n' "$*" >&2; exit 1; }

command -v relay >/dev/null || die "未找到 relay CLI(先 make install)"

# 读 shared config 中 backend.config.<key> 的标量值
cfg_bc() {
  local k="$1" mode=0
  awk -v key="$k" '
    /^[[:space:]]*backend:/  {mode=1; next}
    mode && /^[[:space:]]*config:/ {mode=2; next}
    mode==2 && /^[[:space:]]*[A-Za-z_]+:/ && $0 !~ ("^[[:space:]]*" key "[[:space:]]*:") {exit}
    mode==2 && match($0, "^[[:space:]]*" key "[[:space:]]*:[[:space:]]*") {
        v=substr($0, RSTART+RLENGTH); gsub(/^["'"'"']|["'"'"'][[:space:]]*$|[[:space:]]*(#.*)?$/, "", v); print v; exit
    }' "$CONFIG"
}

# 未指定 -w 时取首个 workspace
pick_watch() {
  if [[ -n "$WATCH" ]]; then echo "$WATCH"; return; fi
  local first; first=$(relay -c "$CONFIG" ws 2>/dev/null | head -1 || true)
  [[ -n "$first" ]] && echo "$first" || die "无可用 workspace,请设 RELAY_WATCH=<id>"
}

# 远端执行一次命令(不带 -w,避开 exec 健康检查的 "Checking..." 输出)
remote_cmd() { relay exec -c "$CONFIG" "$1" 2>&1 | sed -E 's/^Checking remote watcher\.\.\. OK//'; }

# ---- 1) 探测远端 OS/arch ----
detect_remote() {
  log "探测远端系统 ..."
  local name; name=$(remote_cmd "uname -s"  | tail -1 | tr -d '\r')
  local arch; arch=$(remote_cmd "uname -m"  | tail -1 | tr -d '\r' || true)
  case "$name" in
    *MINGW*|*MSYS*|*CYGWIN*) REMOTE_OS=windows; REMOTE_BIN=relay.exe ;;
    Linux)                   REMOTE_OS=linux;   REMOTE_BIN=relay ;;
    *) warn "未识别系统(uname='$name'),默认 linux/amd64"; REMOTE_OS=linux; REMOTE_BIN=relay; arch=amd64 ;;
  esac
  # 自动探测远端正在运行的 relay 二进制路径(换装目标),不猜安装目录。
  REMOTE_BIN_PATH="$(remote_cmd "command -v relay" | tail -1 | tr -d '\r')"
  log "远端: $REMOTE_OS ($arch) -> 目标二进制 $REMOTE_BIN (running at: ${REMOTE_BIN_PATH:-未知})"
}

# ---- 2) 本地交叉编译 ----
build_binary() {
  log "交叉编译 $REMOTE_BIN ..."
  local target
  case "$REMOTE_OS" in
    windows) target="GOOS=windows GOARCH=amd64" ;;
    linux)   target="GOOS=linux   GOARCH=amd64" ;;
  esac
  env CGO_ENABLED=0 $target go build -ldflags="-s -w $STAMP" -o "$REMOTE_BIN" ./cmd/relay
  [[ -f "$REMOTE_BIN" ]] || die "构建失败: $REMOTE_BIN"
  # 用独立文件名下发,避免覆盖远端正在运行的同名可执行文件(Windows 上运行中文件会被占用)。
  REMOTE_STAGED="$REMOTE_BIN.staged"
  cp -f "$REMOTE_BIN" "$REMOTE_STAGED"
}

# ---- 3) 经中转 relay transport 纯下发到执行端二进制旁(绝对路径,不跑 workspace job) ----
push_binary() {
  # 目标:自动探测到的运行二进制旁的新文件(独立名,避免覆盖运行中的同名 exe)
  local dest="${REMOTE_BIN_PATH}.new"
  log "经中转 transport 下发 $REMOTE_STAGED -> 执行端 $dest ..."
  # transport 不传 -w(自动从版本台账挑执行方 watch);不触发 workspace job。
  relay transport -c "$CONFIG" "$REMOTE_STAGED" "$dest"
  REMOTE_NEW="$dest"
}

# ---- 4) detached 换装重启(RESTART=1;替换自动探测到的 REMOTE_BIN_PATH) ----
restart_binary() {
  [[ "$RESTART" == "1" ]] || {
    warn "RESTART 未开:已下发 $REMOTE_NEW,未换远端。需要自动替换+重启: make deploy-remote RESTART=1"
    return 0
  }
  local w="$1" st dest
  st="${REMOTE_NEW:-${REMOTE_BIN_PATH}.new}"
  dest="${REMOTE_BIN_PATH:-}"
  log "触发 detached 换装 (new=$st -> dest=${dest:-<远端 command -v relay>}, RESTART=1) ..."
  read -r -d '' RCMD <<EOF || true
STG='$st'
DEST='$dest'
# detached: 先返回本 exec 响应,3s 后停旧->换->起
( sleep 3
  [ -z "\$DEST" ] && DEST="\$(command -v relay)"; [ -n "\$DEST" ] || DEST="\$HOME/.local/bin/relay"
  # 平台相关的可靠终止:Windows 用 taskkill 按镜像名清掉所有 relay(msys kill 常失效);
  # POSIX 用 pkill。确保不留堆叠的旧 daemon / 控制台窗口。
  # 注意:MSYS_NO_PATHCONV=1 下必须用单斜杠 /F,双斜杠 //F 会给 system32 的 taskkill(解析不了)。
  case "\$(uname -s 2>/dev/null)" in
    *MINGW*|*MSYS*|*CYGWIN*) MSYS_NO_PATHCONV=1 taskkill /F /IM relay.exe >/dev/null 2>&1 || true ;;
    *) pkill -9 -f 'relay watch' 2>/dev/null || true ;;
  esac
  sleep 1
  mv -f "\$STG" "\$DEST" && chmod +x "\$DEST"
  nohup "\$DEST" watch -c "\$HOME/.relay/config.yaml" >>"\$HOME/.relay/relay.log" 2>&1 &
  echo "restarted \$DEST"
) &
echo "swap scheduled; new staged at \$STG"
EOF
  relay exec -c "$CONFIG" -w "$w" "$RCMD"
  log "换装已调度(约 3s 后自动替换+重启)日志: ~/.relay/relay.log"
}

# ---- 6) 中转手工清单 ----
# 中转的 relay 安装路径无法经 relay 探测(中转非 executor、relay 只做文件交换),
# 由 TRANSIT_BIN 指定(默认 ~/.local/bin/relay);部署后可用 relay version -r 核验。
transit() {
  log "构建中转二进制 relay-linux ..."
  env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w $STAMP" -o relay-linux ./cmd/relay
  TRANSIT_BIN="${TRANSIT_BIN:-$HOME/.local/bin/relay}"
  cat <<EOF

=== 中转服务器更新(半自动:经 code-server web 人工) ===
中转无程序化通道,需手工:
  1. 已生成 relay-linux,经 code-server web 上传到中转可达目录(e.g. $HOME/relay/)
  2. 用 self-update + reboot 一键替换重启(新二进制已带 relay server upgrade):
       cp -f relay-linux $TRANSIT_BIN && chmod +x $TRANSIT_BIN
       relay server upgrade $TRANSIT_BIN -c $HOME/.relay/config.yaml
     # 或仅重启不换版本: relay server restart -c $HOME/.relay/config.yaml
  状态: relay server status -c $HOME/.relay/config.yaml
  说明: 中转路径无法经 relay 自动探测(无命令通道),如需换目录请 export TRANSIT_BIN=<path> 重跑。
EOF
}

# ---- 7) 部署后核验:三端版本台账 ----
verify() {
  log "核验版本台账 (relay version -r) ..."
  relay version -r -c "$CONFIG"
}

cmd="${1:-all}"
case "$cmd" in
  remote) W="$(pick_watch)"; detect_remote; build_binary; push_binary "$W"; restart_binary "$W"; verify ;;
  transit) transit ;;
  all) W="$(pick_watch)"; detect_remote; build_binary; push_binary "$W"; restart_binary "$W"; warn "=== 中转段 ==="; transit; verify ;;
  *) die "用法: $0 {remote|transit|all}" ;;
esac
log "完成。"