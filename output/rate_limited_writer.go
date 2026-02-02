package output

import (
	"io"
	"sync"
	"time"
)

type RateLimitedWriter struct {
	writer         io.Writer
	bytesPerSecond int
	startTime      time.Time
	bytesWritten   int64
	mu             sync.Mutex
}

func NewRateLimitedWriter(writer io.Writer, bytesPerSecond int) *RateLimitedWriter {
	return &RateLimitedWriter{
		writer:         writer,
		bytesPerSecond: bytesPerSecond,
		startTime:      time.Now(),
	}
}

func (rlw *RateLimitedWriter) Write(p []byte) (int, error) {
	rlw.mu.Lock()
	defer rlw.mu.Unlock()
	if rlw.bytesPerSecond <= 0 {
		n, err := rlw.writer.Write(p)
		rlw.bytesWritten += int64(n)
		return n, err
	}

	// Rebase periodically to keep durations small.
	if time.Since(rlw.startTime) > time.Minute {
		rlw.startTime = time.Now()
		rlw.bytesWritten = 0
	}

	written := 0
	for len(p) > 0 {
		n, err := rlw.writer.Write(p)
		if n > 0 {
			written += n
			rlw.bytesWritten += int64(n)
			// Sleep after writing to maintain an average pace.
			target := rlw.startTime.Add(time.Duration(rlw.bytesWritten) * time.Second / time.Duration(rlw.bytesPerSecond))
			if wait := time.Until(target); wait > 0 {
				time.Sleep(wait)
			}
			p = p[n:]
		}
		if err != nil {
			return written, err
		}
		if n == 0 {
			return written, io.ErrShortWrite
		}
	}

	return written, nil
}
