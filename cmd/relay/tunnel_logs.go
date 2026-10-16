package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/user/relay/internal/daemon"
)

// shortenPath 把家目录替换为 ~ 缩短显示
func shortenPath(path string) string {
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		if strings.HasPrefix(path, u.HomeDir) {
			return "~" + strings.TrimPrefix(path, u.HomeDir)
		}
	}
	return path
}

// tunnelLogLineMatches 判断一行日志是否属于指定 egress。
func tunnelLogLineMatches(line, egress string) bool {
	return strings.Contains(line, "[tunnel "+egress+"]")
}

// forwardLogLineMatches 判断一行日志是否属于指定端口的独立转发。
func forwardLogLineMatches(line string, port int) bool {
	return strings.Contains(line, "[forward "+strconv.Itoa(port)+"]")
}

// readTunnelLogTail 有界读取 node 日志中匹配 matcher 的最后至多 tailLines 行:从文件末尾按窗口
// 回退读取,命中不足时窗口翻倍(上限为文件大小)。node.log 无轮转、无上限增长,故避免整文件扫描。
func readTunnelLogTail(logPath string, tailLines int, matches func(line string) bool) ([]string, error) {
	f, err := os.Open(logPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := fi.Size()
	if size == 0 {
		return nil, nil
	}

	window := int64(64 * 1024)
	if w := int64(tailLines) * 512; w > window {
		window = w
	}
	for {
		if window > size {
			window = size
		}
		start := size - window
		buf := make([]byte, window)
		if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
			return nil, err
		}
		text := string(buf)
		if start > 0 {
			// 窗口首行可能是被截断的半行,丢弃到第一个换行之后。
			if i := strings.IndexByte(text, '\n'); i >= 0 {
				text = text[i+1:]
			} else {
				text = ""
			}
		}
		var matched []string
		for _, line := range strings.Split(text, "\n") {
			if line != "" && matches(line) {
				matched = append(matched, line)
			}
		}
		if len(matched) >= tailLines || start == 0 {
			if len(matched) > tailLines {
				matched = matched[len(matched)-tailLines:]
			}
			return matched, nil
		}
		window *= 2
	}
}

// showTunnelLogs 把 `relay logs <egress>` 落到 node 日志上,只呈现该隧道的行。调用方须先经
// resolveLifecycle 确认身份(holder 或意图文件认识该 egress)且本机 node 在运行:
// 此时零匹配行是「尚无输出」而非「不是隧道」,照常打印已有行(可能一行都没有)并在 follow 时进入跟读。
// node 前台运行时隧道日志走 stderr、不落 node.log,故仅当日志文件缺失才补一条提示。
func showTunnelLogs(egress string, follow bool, tailLines int) {
	if tailLines <= 0 {
		tailLines = 100
	}
	logPath := daemon.LogFile("node")
	lines, err := readTunnelLogTail(logPath, tailLines, func(line string) bool { return tunnelLogLineMatches(line, egress) })
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "node log %s not found (the node may be running in the foreground; its tunnel logs go to stderr).\n", shortenPath(logPath))
		}
		return
	}
	w := bufio.NewWriter(os.Stdout)
	for _, l := range lines {
		fmt.Fprintln(w, l)
	}
	_ = w.Flush()

	if follow {
		followTunnelLog(logPath, func(line string) bool { return tunnelLogLineMatches(line, egress) })
	}
}

// showForwardLogs 把 `relay logs <port>` 落到 node 日志上,只呈现该独立转发的行。
// 调用方须先经 resolveLifecycle 确认身份且本机 node 在运行(镜像 showTunnelLogs)。
func showForwardLogs(port int, follow bool, tailLines int) {
	if tailLines <= 0 {
		tailLines = 100
	}
	logPath := daemon.LogFile("node")
	lines, err := readTunnelLogTail(logPath, tailLines, func(line string) bool { return forwardLogLineMatches(line, port) })
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "node log %s not found (the node may be running in the foreground; its forward logs go to stderr).\n", shortenPath(logPath))
		}
		return
	}
	w := bufio.NewWriter(os.Stdout)
	for _, l := range lines {
		fmt.Fprintln(w, l)
	}
	_ = w.Flush()

	if follow {
		followTunnelLog(logPath, func(line string) bool { return forwardLogLineMatches(line, port) })
	}
}

// followTunnelLog 轮询 node 日志新增内容,只打印匹配 matcher 的行,直到收到 SIGINT/SIGTERM。
// 打开一次并持有 fd,每 tick 仅 Stat + 续读;仅在出错或日志被截断时重新定位。
func followTunnelLog(logPath string, matches func(line string) bool) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	f, err := os.Open(logPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "follow tunnel log: %v\n", err)
		return
	}
	defer f.Close()

	// 从当前文件末尾起追,避免重复打印刚刚 tail 过的内容。
	var offset int64
	if fi, err := f.Stat(); err == nil {
		offset = fi.Size()
	}
	pending := "" // 跨轮次的半行缓冲

	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	buf := make([]byte, 32*1024)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fi, err := f.Stat()
			if err != nil {
				return
			}
			if fi.Size() < offset { // rotate/truncate:从头再读
				if _, err := f.Seek(0, io.SeekStart); err != nil {
					return
				}
				offset, pending = 0, ""
			}
			if fi.Size() == offset {
				continue
			}
			if _, err := f.Seek(offset, io.SeekStart); err != nil {
				return
			}
			var chunk []byte
			for {
				n, rerr := f.Read(buf)
				if n > 0 {
					chunk = append(chunk, buf[:n]...)
					offset += int64(n)
				}
				if rerr != nil {
					break
				}
			}
			pending += string(chunk)
			for {
				idx := strings.IndexByte(pending, '\n')
				if idx < 0 {
					break
				}
				line := pending[:idx]
				pending = pending[idx+1:]
				if matches(line) {
					fmt.Println(line)
				}
			}
		}
	}
}
