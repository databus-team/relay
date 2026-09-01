// Package logx 提供进程级 verbose 日志开关。所有在 --debug 下才该输出的
// 审计/诊断日志统一走 Debugf,便于用单一标志(grep --debug)开关与过滤。
//
// Info 级核心生命周期日志仍用标准库 log(由 cmd/relay 统一加上时间戳)。
package logx

import "log"

// verbose 由 --debug 打开;为 true 时 Debugf 才输出。
var verbose bool

// SetDebug 打开/关闭 verbose 日志。cmd/relay 在 main 里按 --debug 调用。
func SetDebug(on bool) { verbose = on }

// Debug 返回 verbose 是否开启(调用方需要自留分支时用)。
func Debug() bool { return verbose }

// Debugf 仅在 verbose 下输出,统一带 [debug] 前缀便于 grep 过滤。
func Debugf(format string, args ...interface{}) {
	if verbose {
		log.Printf("[debug] "+format, args...)
	}
}
