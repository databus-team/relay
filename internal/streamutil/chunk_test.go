package streamutil

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// 分片边界切开多字节字符时,半截字节必须留到下一片。
func TestSplitIncompleteRune(t *testing.T) {
	// "中文" = E4 B8 AD E6 96 87
	full := []byte("中文")

	tests := []struct {
		name     string
		in       string
		wantHead string
		wantTail string
	}{
		{name: "ascii 原样", in: "abc", wantHead: "abc"},
		{name: "整字完整", in: "a中", wantHead: "a中"},
		{name: "截断在 3 字节序列中间(1)", in: "a\xe4", wantHead: "a", wantTail: "\xe4"},
		{name: "截断在 3 字节序列中间(2)", in: "a\xe4\xb8", wantHead: "a", wantTail: "\xe4\xb8"},
		{name: "截断在 4 字节序列中间", in: "a\xf0\x9f\x98", wantHead: "a", wantTail: "\xf0\x9f\x98"},
		{name: "非法字节原样送出", in: "a\xff", wantHead: "a\xff"},
		{name: "空的", in: "", wantHead: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			head, tail := SplitIncompleteRune([]byte(tt.in))
			if string(head) != tt.wantHead || string(tail) != tt.wantTail {
				t.Errorf("SplitIncompleteRune(%q) = (%q, %q), want (%q, %q)",
					tt.in, head, tail, tt.wantHead, tt.wantTail)
			}
		})
	}

	// 逐字节喂进去,拼回来必须是完整的中文。
	var pending []byte
	var out []byte
	for i := 0; i < len(full); i++ {
		pending = append(pending, full[i])
		head, tail := SplitIncompleteRune(pending)
		out = append(out, head...)
		pending = append(pending[:0], tail...)
	}
	out = append(out, pending...)
	if string(out) != "中文" {
		t.Errorf("逐字节拼接结果 = %q, want 中文", out)
	}
	if !utf8.Valid(out) {
		t.Errorf("拼接结果不是合法 UTF-8: %q", out)
	}
}

// 大块分片也不该产生替换字符。
func TestSplitIncompleteRune_NoReplacementChar(t *testing.T) {
	line := strings.Repeat("中文测试", 100)
	blob := []byte(strings.Repeat(line+"\n", 50))
	var pending []byte
	var sb strings.Builder
	for off := 0; off < len(blob); off += 7 { // 故意用非 3 的倍数切
		end := off + 7
		if end > len(blob) {
			end = len(blob)
		}
		pending = append(pending, blob[off:end]...)
		head, tail := SplitIncompleteRune(pending)
		sb.Write(head)
		pending = append(pending[:0], tail...)
	}
	sb.Write(pending)
	if strings.ContainsRune(sb.String(), utf8.RuneError) {
		t.Errorf("输出里出现 U+FFFD")
	}
	if sb.String() != string(blob) {
		t.Errorf("往返内容不一致: got %d 字节, want %d 字节", sb.Len(), len(blob))
	}
}
