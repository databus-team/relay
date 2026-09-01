// Package version 承载一次构建的元信息(版本/提交/构建时间)。
//
// 这些字段由构建产物在编译期通过 -ldflags -X 打入(见 Makefile 的 VERSION/
// COMMIT/DATE),所有节点(本地调用方 / 中转 server / 远端执行方 watch)共享同一
// 份代码路径,因此打包到各自二进制里的就是各自的真实构建信息,便于 `relay version`
// 跨机对比版本是否一致。未 stamp 的开发构建(go build)显示 Version=dev。
package version

import "fmt"

var (
	// Version 语义版本或 git describe 结果;默认 dev。
	Version = "dev"
	// Commit 短提交 hash,便于定位到具体源码。
	Commit = ""
	// Date 构建时间(UTC RFC3339)。
	Date = ""
)

// String 返回紧凑版本串(带 commit/hash)用于日志与信息统统一。
func String() string {
	if Commit == "" {
		return Version
	}
	return fmt.Sprintf("%s+%s", Version, Commit)
}

// Full 返回含构建时间的完整版本串。
func Full() string {
	base := String()
	if Date != "" {
		base += " (" + Date + ")"
	}
	return base
}
