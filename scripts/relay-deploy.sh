#!/usr/bin/env bash
#
# relay 一键部署助手
#
# 两条腿:
#   远端(executors) —— 默认把新二进制下发到 `relay status` 中**在线**的全部执行器
#                     (多执行器各自 executor_id,逐一探测 OS/arch、交叉编译、下发)。
#                     离线执行器主动跳过(反正推不上),避免误推。可用 RELAY_EXECUTORS
#                     收窄到其中几台(逗号/空格分隔的 executor_id;只保留指定且在线者)。
#                     RESTART=1 时自动 detached 换装并重启(先停->换->起,且不能重启到
#                     正在服务本条的 relay,故走则分离执行),并逐台核验已切到新提交。
#   中转(transit) —— 一键:经 `relay server-remote` 受控自升级通道把新构建的 relay-linux
#                  流式交付中转(中转本地自检 → 回执 → 换装 .prev备份/重启),本地断连
#                  重连轮询版本核验。仅放开「自我升级」一条窄径,中转不会自注册为执行器。
#                  首跳依赖:在跑旧构建的中转需一次手工 seed(relay server upgrade)。
#
# 用法:
#   RESTART=1 make deploy-remote     # 部署全部在线执行器 + 自动替换重启
#   make deploy-remote               # 只下发,不替换重启(安全)
#   RESTART=1 RELAY_EXECUTORS=site-a make deploy-remote   # 只部署 site-a
#   make deploy-transit              # 一键部署中转(受控自升级 + 版本核验)
#   make deploy                      # 先执行器后中转,全跑
#
# 依赖:`relay` CLI 在 PATH;配置为共享 relay 配置;执行器用同一 config 以 `relay watch` 跑。
# 定位某台执行器时,CLI 直接按 `--executor <executor_id>` 节点寻址(节点身份,无需 workspace 绑定),故
# 未绑定任何 workspace 的执行器(如 extranet)同样可被部署/操作。
set -euo pipefail

CONFIG="${RELAY_CONFIG:-$HOME/.relay/config.yaml}"
RESTART="${RESTART:-0}"
# 可选:只部署其中几台执行器(逗号/空格分隔的 executor_id;仅保留指定且在线的)。
EXECS="${RELAY_EXECUTORS:-}"

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

# 在指定执行器上执行一条命令。直接按 executor_id 寻址(--executor 节点直连,不再依赖
# workspace 绑定;未绑定 workspace 的执行器同样可被部署/操作)。
remote_cmd() { "$(command -v relay)" exec -c "$CONFIG" --executor "$1" "${@:2}" 2>&1 | sed -E 's/^Checking remote watcher\.\.\. OK//'; }

# 枚举在线执行器:`relay status --json` 的 executors[] 里每台的 executor_id。
list_online_executors() {
  relay status --json -c "$CONFIG" 2>/dev/null \
    | awk 'match($0,/"executor_id": *"[^"]*"/){s=substr($0,RSTART,RLENGTH); split(s,a,"\""); print a[4]}'
}

# ---- 1) 探测远端 OS/arch ----
detect_remote() {
  local e="$1"
  log "探测远端系统 ..."
  local name; name=$(remote_cmd "$e" "uname -s"  | tail -1 | tr -d '\r')
  local arch; arch=$(remote_cmd "$e" "uname -m"  | tail -1 | tr -d '\r' || true)
  case "$name" in
    *MINGW*|*MSYS*|*CYGWIN*) REMOTE_OS=windows; REMOTE_BIN=relay.exe ;;
    Linux)                   REMOTE_OS=linux;   REMOTE_BIN=relay ;;
    *) warn "未识别系统(uname='$name'),默认 linux/amd64"; REMOTE_OS=linux; REMOTE_BIN=relay; arch=amd64 ;;
  esac
  # 自动探测该平台的运行中 relay 二进制路径(换装目标),不猜安装目录。
  REMOTE_BIN_PATH="$(remote_cmd "$e" "command -v relay" | tail -1 | tr -d '\r')"
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

# ---- 3) 经中转 relay push --no-jobs --dest 下发到执行端二进制旁(绝对路径,不跑 workspace job) ----
push_binary() {
  local e="$1"
  # 目标:自动探测到的运行二进制旁的新文件(独立名,避免覆盖运行中的同名 exe)
  local dest="${REMOTE_BIN_PATH}.new"
  log "经中转 push --no-jobs 下发 $REMOTE_STAGED -> 执行端 $dest ..."
  # push --no-jobs 不走 workspace job;--dest 为绝对落盘路径。按 executor_id 直达目标执行器。
  relay push --no-jobs -c "$CONFIG" --executor "$e" --dest "$dest" "$REMOTE_STAGED"
  REMOTE_NEW="$dest"
}

# ---- 4) detached 换装重启(RESTART=1;替换自动探测到的 REMOTE_BIN_PATH) ----
restart_binary() {
  [[ "$RESTART" == "1" ]] || {
    warn "RESTART 未开:已下发 $REMOTE_NEW,未换远端。需要自动替换+重启: make deploy-remote RESTART=1"
    return 0
  }
  local e="$1" st dest
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
  relay exec -c "$CONFIG" --executor "$e" "nohup sh -c '$swap' >/dev/null 2>&1 &" \
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
  # 期望版本 = 写入这份 relay-linux 的 stamp(与上面 build 的 ldflags 完全一致)。
  # 不能拿本地进程 version.String() 当期望:本地装的 relay 可能是干净构建,而现场 cross-compile
  # 的 relay-linux 因工作区改动会带 -dirty 后缀(或本地版本更旧),导致核验对账恒等不上而超时。
  local expect="${GIT_VERSION}"
  [[ -n "$GIT_COMMIT" ]] && expect="${GIT_VERSION}+${GIT_COMMIT}"
  # server-remote 内含:上传 → 中转自检/回执 → 换装重启 → 断线重连 → 轮询版本核验(按 expect)。
  if ! relay server-remote -c "$CONFIG" --binary relay-linux --expect "$expect"; then
    warn "中转一键部署失败(运行中实例不受影响、.prev 备件已保留)。请人工回退后重试:"
    warn "  兜底:经 code-server 上传 relay-linux,再执行 relay server upgrade $HOME/.local/bin/relay -c $CONFIG"
    return 1
  fi
  log "中转已运行新版本 ✔"
}

# ---- 7) 部署后核验:某台执行器是否已切换为新 commit ----
# 从 `relay status --json` 里定位 executor_id==<exec> 的那台执行器,校验其 version
# 已带本次构建的 $GIT_COMMIT。仅 RESTART=1 时调用(未换装则仍是旧构建,会超时,属预期)。
verify_executor() {
  local id="${1:?需要 <executor_id>}" want="${2:-$GIT_COMMIT}" i ver
  # RESTART=1 换装后执行器 daemon 约 3s 后被杀重启,给足时间并容忍瞬时不可达。
  sleep 3
  log "核验执行器 $id 版本 (status --json, 等待新提交 $want, 至 30s) ..."
  for i in $(seq 1 15); do
    ver="$(relay status --json -c "$CONFIG" 2>/dev/null \
      | awk -v id="$id" '
          /"executor_id":/{c=""; if(match($0,/"executor_id": *"[^"]*"/)){s=substr($0,RSTART,RLENGTH); split(s,a,"\""); c=a[4]}}
          /"version":/ && c==id && match($0,/"version": *"[^"]*"/){s=substr($0,RSTART,RLENGTH); split(s,a,"\""); print a[4]; exit}
        ')"
    if grep -q "+${want}" <<<"$ver"; then
      log "执行器 $id 已切到新提交 $want ✔"
      return 0
    fi
    log "  重试 $i/15 等待 $id 切到新提交 ..."
    sleep 2
  done
  log "核验超时:执行器 $id 未切到新提交 $want(RESTART=0 未换装时属预期)。"
  return 1
}

# ---- 远端段:枚举在线执行器并逐一部署(按 executor_id 直接寻址,无需 workspace 绑定) ----
remote_all() {
  local E rc=0 i line
  local -a EXEC_IDS wanted
  # 不用 mapfile(Bash 3.2 的 macOS /bin/bash 无此内建),用 while read 逐前行组数组。
  EXEC_IDS=()
  while IFS= read -r line; do [[ -n "$line" ]] && EXEC_IDS+=("$line"); done < <(list_online_executors)
  if [[ -n "${EXECS:-}" ]]; then
    # 用户点名(逗号/空格分隔的 executor_id)且在线的那部分;点名的离线也跳过。
    wanted=()
    while IFS= read -r line; do [[ -n "$line" ]] && wanted+=("$line"); done < <(printf '%s\n' ${EXECS//[,]/ })
    exec_online="$(printf '%s\n' "${EXEC_IDS[@]}")"
    EXEC_IDS=()
    for E in "${wanted[@]}"; do grep -qx "$E" <<<"$exec_online" && EXEC_IDS+=("$E"); done
  fi
  [[ "${#EXEC_IDS[@]}" -eq 0 ]] && { warn "无在线执行器(或 RELAY_EXECUTORS 已指明但全部离线),跳过远端段。"; return 0; }
  log "本次部署执行器: ${EXEC_IDS[*]}"
  for E in "${EXEC_IDS[@]}"; do
    log "=== 部署执行器 $E ==="
    if ! detect_remote "$E"; then warn "探测 $E 失败,跳过"; rc=1; continue; fi
    if ! build_binary;   then warn "交叉编译失败($E),跳过";         rc=1; continue; fi
    if ! push_binary "$E"; then warn "下发 $E 失败,跳过";            rc=1; continue; fi
    if [[ "$RESTART" == "1" ]]; then
      restart_binary "$E"
      verify_executor "$E" "$GIT_COMMIT" || rc=1
    fi
  done
  [[ "$rc" -eq 0 ]] || die "部分执行器部署失败。"
}

cmd="${1:-all}"
case "$cmd" in
  remote) remote_all ;;
  transit) transit ;;
  all) remote_all; warn "=== 中转段 ==="; transit ;;
  *) die "用法: $0 {remote|transit|all}" ;;
esac
log "完成。"