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
  local name arch
  name=$(remote_cmd "uname -s"  | tail -1 | tr -d '\r')
  arch=$(remote_cmd "uname -m"  | tail -1 | tr -d '\r' || true)
  case "$name" in
    *MINGW*|*MSYS*|*CYGWIN*) REMOTE_OS=windows; REMOTE_BIN=relay.exe ;;
    Linux)                   REMOTE_OS=linux;   REMOTE_BIN=relay ;;
    *) warn "未识别系统(uname='$name'),默认 linux/amd64"; REMOTE_OS=linux; REMOTE_BIN=relay; arch=amd64 ;;
  esac
  log "远端: $REMOTE_OS ($arch) -> 目标二进制 $REMOTE_BIN"
}

# ---- 2) 本地交叉编译 ----
build_binary() {
  log "交叉编译 $REMOTE_BIN ..."
  local target
  case "$REMOTE_OS" in
    windows) target="GOOS=windows GOARCH=amd64" ;;
    linux)   target="GOOS=linux   GOARCH=amd64" ;;
  esac
  env CGO_ENABLED=0 $target go build -ldflags='-s -w' -o "$REMOTE_BIN" ./cmd/relay
  [[ -f "$REMOTE_BIN" ]] || die "构建失败: $REMOTE_BIN"
}

# ---- 3) 经中转 push 下发 ----
push_binary() {
  local w="$1"
  log "经中转 push $REMOTE_BIN -> workspace[$w] ..."
  relay push -c "$CONFIG" -w "$w" "$REMOTE_BIN"
}

# ---- 4) 远端 staged 路径(由共享 config 精确推导) ----
staged_path() {
  local w="$1" exe watch_dir
  exe="$(cfg_bc executor_dir)"; exe="${exe:-$HOME}"
  watch_dir="$(awk -v id="$w" '
    $0 ~ "^[[:space:]]*- id: *"id"$" {idc=1; next}
    idc && /^[[:space:]]*watch_dir:/ {sub(/^[[:space:]]*watch_dir:[[:space:]]*/,""); gsub(/"/,""); v=$0; exit} \
    END{print v? v : "."}' "$CONFIG")"
  # watch_dir 若是 "." 则直接落 executor 根
  if [[ "$watch_dir" == "." || -z "$watch_dir" ]]; then
    echo "$exe/$REMOTE_BIN"
  else
    echo "$exe/$watch_dir/$REMOTE_BIN"
  fi
}

# ---- 5) detached 换装重启(missing RESTART=1 时跳过) ----
restart_binary() {
  [[ "$RESTART" == "1" ]] || {
    warn "RESTART 未开:已下发本地 $REMOTE_BIN,未重启远端。需要自动替换+重启: make deploy-remote RESTART=1"
    return 0
  }
  local w="$1" st
  st="$(staged_path "$w")"
  log "触发 detached 换装 (staged=$st, RESTART=1) ..."
  read -r -d '' RCMD <<EOF || true
STG='$st'
# detached:先返回本 exec 响应,3s 后停->换->起
( sleep 3
  BIN="\$(command -v relay)"; [ -n "\$BIN" ] || BIN="\$HOME/.local/bin/relay"
  for p in \$(ps -eo pid=,args= 2>/dev/null | grep '[r]elay watch' | awk '{print \$1}'); do
    kill -9 "\$p" 2>/dev/null || true
  done
  cp -f "\$STG" "\$BIN" && chmod +x "\$BIN"
  nohup "\$BIN" watch -c "\$HOME/.relay/config.yaml" >>"\$HOME/.relay/relay.log" 2>&1 &
  echo "restarted \$BIN"
) &
echo "swap scheduled; new staged at \$STG"
EOF
  relay exec -c "$CONFIG" -w "$w" "$RCMD"
  log "换装已调度(约3s后自动替换+重启)日志: ~/.relay/relay.log"
}

# ---- 6) 中转手工清单 ----
transit() {
  log "构建中转二进制 relay-linux ..."
  env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags='-s -w' -o relay-linux ./cmd/relay
  cat <<"EOF"

=== 中转服务器更新(半自动:经 code-server web 人工) ===
中转无程序化通道,需手工:
  1. 已生成 relay-linux,经 code-server web 上传到中转可达目录(e.g. $HOME/relay/)
  2. 替换二进制并重启(可用新 daemon 形式):
       cp -f relay-linux ~/.local/bin/relay && chmod +x ~/.local/bin/relay
       relay server restart -c ~/.relay/config.yaml   # daemon 重启(旧进程被 SIGTERM)
       # 或前台: relay server -c ~/.relay/config.yaml
  状态查看: relay server status -c ~/.relay/config.yaml
EOF
}

cmd="${1:-all}"
case "$cmd" in
  remote) W="$(pick_watch)"; detect_remote; build_binary; push_binary "$W"; restart_binary "$W" ;;
  transit) transit ;;
  all) W="$(pick_watch)"; detect_remote; build_binary; push_binary "$W"; restart_binary "$W"; warn "=== 中转段 ==="; transit ;;
  *) die "用法: $0 {remote|transit|all}" ;;
esac
log "完成。"