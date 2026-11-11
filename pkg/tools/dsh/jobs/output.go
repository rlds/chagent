package jobs

import (
	"io"
	"os"
	"sync"
	"sync/atomic"
)

// cappedWriter 将任务输出写入 output.log，最多写入 maxBytes 字节，
// 同时统计实际持久化的字节数，并记录输出是否被丢弃。
// stdout 和 stderr 共用同一个 writer，因此每次 Write 都是串行执行的：
// 当目标不是 *os.File 时，os/exec 会用两个 goroutine 分别拷贝它们。
type cappedWriter struct {
	mu          sync.Mutex
	file        *os.File
	max         int64
	writtenN    atomic.Int64
	truncatedIn atomic.Bool
}

func newCappedWriter(file *os.File, max int64) *cappedWriter {
	return &cappedWriter{file: file, max: max}
}

// Write 实现 io.Writer。达到上限时，它绝不会向 os/exec 报告短写：
// 剩余部分会被计为已丢弃，从而让子进程继续运行，直到退出或被杀死。
func (w *cappedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	written := w.writtenN.Load()
	remaining := w.max - written
	if remaining <= 0 {
		w.truncatedIn.Store(true)
		return len(p), nil
	}
	if int64(len(p)) > remaining {
		n, err := w.file.Write(p[:remaining])
		w.writtenN.Add(int64(n))
		w.truncatedIn.Store(true)
		if err != nil {
			return len(p), err
		}
		return len(p), nil
	}
	n, err := w.file.Write(p)
	w.writtenN.Add(int64(n))
	return n, err
}

// written 报告已持久化到 output.log 的字节数。
func (w *cappedWriter) written() int64 {
	if w == nil {
		return 0
	}
	return w.writtenN.Load()
}

// truncated 报告是否有输出因达到上限而被丢弃。
func (w *cappedWriter) truncated() bool {
	if w == nil {
		return false
	}
	return w.truncatedIn.Load()
}

var _ io.Writer = (*cappedWriter)(nil)
