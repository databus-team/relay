#!/usr/bin/env bash
#
# relay 一键部署助手
#
# 两条腿:
#   远端(exec)  —— 自动:经中转 `relay push`/`relay exec` 把新二进制下发到远端执行器;
#                    自动探测执行器 OS/arch 并交叉编译对应版本。
#                    RESTART=1 时自动 detached 换装并重启(先停->换->起,且不能砍
#                    掉正在服务本连接的 relay,故延迟后分离执行)。
#   中转(transit) —— 一键:经 `relay server-remote` 受控自升级通道把新构建的 relay-linux
#                  流式交付中转(中转本地自检 → 回执 → 换装 .prev备份/重启),本地断连
#                  重连轮询版本核验。仅放开「自我升级」一条窄径,中转不获得任意执行能力。
#                  首跳依赖:在跑旧构建的中转需一次手工 seed(relay server upgrade)。
#
# 用法:
#   RESTART=1 make deploy-remote    # 部署远端 + 自动替换重启
#   make deploy-remote              # 只下发,不重启(安全)
#   make deploy-transit             # 一键部署中转(受控自升级 + 版本核验)
#   make deploy                     # 全跑
#
# 依赖:`relay` CLI 在 PATH;配置为共享 config;远端用同一 config 以 executor 跑 relay watch。
set -euo pipefail

CONFIG="${RELAY_CONFIG:-$HOME/.relay/config.yaml}"
WATCH="${RELAY_WATCH:-}"
RESTART="${RESTART:-0}"

# 版本 stamp:与 Makefile 一致,保证产出的远端/中转二进制带同一标识,relay status 才能正确对比。
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
  # Windows:git-bash 的 `command -v relay` 常回显不带扩展名的 "…/relay",
  # 但真正能起 daemon 的必须是 relay.exe。缺 .exe 时补上,否则换装后
  # `watch start` 起不来(executable file not found in $PATH)。
  if [[ "$REMOTE_OS" == windows && -n "$REMOTE_BIN_PATH" && "${REMOTE_BIN_PATH##*.}" != "exe" ]]; then
    REMOTE_BIN_PATH="${REMOTE_BIN_PATH}.exe"
  fi
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

# ---- 3) 经中转 relay push --no-jobs --dest 纯下发到执行端二进制旁(绝对路径,不跑 workspace job) ----
push_binary() {
  # 目标:自动探测到的运行二进制旁的新文件(独立名,避免覆盖运行中的同名 exe)
  local dest="${REMOTE_BIN_PATH}.new"
  log "经中转 push --no-jobs 下发 $REMOTE_STAGED -> 执行端 $dest ..."
  # push --no-jobs 不走 workspace job;--dest 为绝对落盘路径。-w 用当前工作区即可。
  relay push --no-jobs -c "$CONFIG" -w "$W" --dest "$dest" "$REMOTE_STAGED"
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
  log "换装 $st -> $dest (RESTART=1)..."
  # 不用 `watch upgrade` 换装:它会对「运行中的 exe」做自覆盖,Windows 下失败,
  # 会旧 daemon 已停、新的又没起来 -> watch 卡在 stopped。
  # 改用外部 sh 一次完成「停旧 -> 挪走旧 -> 挪入新 -> 起新」,与自替换无关:
  #   watch stop   停旧 daemon(释放仍映射旧 exe 的进程)
  #   mv dest -> dest.prev / st -> dest(旧换出、新的就位)
  #   watch start  以守护进程重启新 daemon(detach + 写 pid,无黑窗)
  # 注意:该 exec 由旧 daemon 服务,watch stop 停掉它后回包会断,链仍随 exec
  # 解耦继续完成(detached),结果由 verify(relay status) 兜底核对。
  local swap
  # $dest/$st 在本地展开成路径值;$HOME 留给远端展开,故写成 \$HOME。
  swap="\"$dest\" watch stop -c \"\$HOME/.relay/config.yaml\" ; "
  swap+="mv -f \"$dest\" \"$dest.prev\" ; "
  swap+="mv -f \"$st\" \"$dest\" ; "
  swap+="\"$dest\" watch start -c \"\$HOME/.relay/config.yaml\""
  # 整段在远端以 detached 后台(nohup … &)触发,让本条 exec 立即返回。
  # 若在前台跑,`watch stop` 会先杀掉服务本 exec 的旧 daemon,导致本地 relay
  # exec 永等其回包而卡死;换装本身并不依赖该连接,由 verify 兜底核对即可。
  relay exec -c "$CONFIG" -w "$w" "nohup sh -c '$swap' >/dev/null 2>&1 &" \
    || warn "换装已在远端后台触发;结果以 verify (relay status) 为准"
  log "换装已后台触发;远端日志: ~/.relay/watch.log"
}

# ---- 6) 中转一键部署(受控自升级) ----
# 构建 relay-linux 并经 `relay server-remote` 经受控自升级通道流式交付中转,由中转本地
# 自检 → 回执 → 换装(.prev 备份 + 重启)。核验随 `relay server-remote` 内部完成
# (断线重连后轮询中转版本台账确认新构建),故中转段无需在此再核验。
transit() {
  log "构建中转二进制 relay-linux ..."
  env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w $STAMP" -o relay-linux ./cmd/relay
  [[ -f relay-linux ]] || die "构建中转二进制失败: relay-linux"

  log "经受控自升级把 relay-linux 一键部署到中转(自检→换装→核验)..."
  # server-remote 内含:上传 → 中转自检/回执 → 换装重启 → 断线重连 → 轮询版本核验。
  if ! relay server-remote -c "$CONFIG" --binary relay-linux; then
    warn "中转一键部署失败(运行中实例不受影响、.prev 备件已保留)。请人工回退后重试:"
    warn "  兜底:经 code-server 上传 relay-linux,再执行 relay server upgrade $HOME/.local/bin/relay -c $CONFIG"
    return 1
  fi
  log "中转已运行新版本 ✔"
}

# ---- 7) 部署后核验:执行方是否已切换为新提交(relay status) ----
# 旧 `relay version -r` 已并入 `relay status`;此处用其 --json 台账,校验执行方(远端)的
# commit 已切到本次构建的 $GIT_COMMIT。仅 RESTART=1 时调用(未换装则远端仍是旧构建,会超时,
# 属预期)。中转段由 `relay server-remote` 内部自检核验,不需在此重复。
verify() {
  local w="${1:?需要 -w 以指认被核验的执行方}" want="${2:-$GIT_COMMIT}" i js
  # RESTART=1 换装后执行方 daemon 约 3s 后被杀重启,给足时间并容忍瞬时不可达。
  sleep 3
  log "核验执行方版本 (relay status --json, 等待新提交 $want, 至多 ~60s) ..."
  for i in $(seq 1 15); do
    js="$(relay status --json -c "$CONFIG" -w "$w" 2>/dev/null)" || { log "  重试 $i/15 status 查询 ..."; sleep 2; continue; }
    # status --json 的 nodes[] 里,执行方节点的 commit 上报为 `"commit": "<GIT_COMMIT>"`。
    if grep -q "\"commit\": \"$want\"" <<<"$js"; then
      log "执行方已返回新提交 $want ✔"
      return 0
    fi
    log "  重试 $i/15 等待执行方切到新提交 ..."
    sleep 2
  done
  log "核验超时:执行方未切到新提交 $want(RESTART=0 未换装时属预期)。"
  return 1
}

cmd="${1:-all}"
case "$cmd" in
  remote) W="$(pick_watch)"; detect_remote; build_binary; push_binary "$W"; restart_binary "$W"
          [[ "$RESTART" == "1" ]] && verify "$W" ;;
  transit) transit ;;
  all) W="$(pick_watch)"; detect_remote; build_binary; push_binary "$W"; restart_binary "$W"; warn "=== 中转段 ==="; transit
          [[ "$RESTART" == "1" ]] && verify "$W" ;;
  *) die "用法: $0 {remote|transit|all}" ;;
esac
log "完成。"