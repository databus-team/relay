// Package streamutil 提供字节流分片工具。
package streamutil

import "unicode/utf8"

// ChunkSize 是流式输出单片读取与发送的字节上限。
const ChunkSize = 32 * 1024

// SplitIncompleteRune 把 b 拆成可安全作为 UTF-8 文本传输的前缀与末尾可能被截断的多字节片段,
// 调用方应把 tail 留到下一片之前。末尾没有半截字符时 tail 为 nil,head 原样返回 b。
func SplitIncompleteRune(b []byte) (head, tail []byte) {
	for i := len(b) - 1; i >= 0 && i > len(b)-utf8.UTFMax; i-- {
		if !utf8.RuneStart(b[i]) {
			continue
		}
		if utf8.FullRune(b[i:]) {
			return b, nil
		}
		return b[:i], b[i:]
	}
	return b, nil
}
