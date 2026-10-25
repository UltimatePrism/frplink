// Package logbuf 提供线程安全的日志环形缓冲，用于 GUI 的日志面板。
package logbuf

import (
	"bytes"
	"sync"
	"time"
)

// Buffer 固定容量的行缓冲。
type Buffer struct {
	mu    sync.Mutex
	lines []string
	max   int
}

// New 创建容量为 max 行的缓冲。
func New(max int) *Buffer {
	if max <= 0 {
		max = 1000
	}
	return &Buffer{max: max}
}

// Write 实现 io.Writer：按行拆分并加盖时间戳。
func (b *Buffer) Write(p []byte) (int, error) {
	n := len(p)
	now := time.Now().Format("15:04:05")
	for _, line := range bytes.Split(bytes.TrimSuffix(p, []byte("\n")), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		b.mu.Lock()
		b.lines = append(b.lines, now+"  "+string(line))
		if len(b.lines) > b.max {
			b.lines = b.lines[len(b.lines)-b.max:]
		}
		b.mu.Unlock()
	}
	return n, nil
}

// Tail 返回最近 n 行。
func (b *Buffer) Tail(n int) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n <= 0 || n > len(b.lines) {
		n = len(b.lines)
	}
	out := make([]string, n)
	copy(out, b.lines[len(b.lines)-n:])
	return out
}
