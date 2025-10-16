package output

import (
	"io"
	"sync"
	"time"
)

type RateLimitedWriter struct {
	writer              io.Writer
	bytesPerSecond      int
	bytesPerMillisecond float64
	startTime           time.Time
	bytesWritten        int64
	mu                  sync.Mutex
}

func NewRateLimitedWriter(writer io.Writer, bytesPerSecond int) *RateLimitedWriter {
	return &RateLimitedWriter{
		writer:              writer,
		bytesPerSecond:      bytesPerSecond,
		bytesPerMillisecond: float64(bytesPerSecond) / 1000.0,
		startTime:           time.Now(),
	}
}

func (rlw *RateLimitedWriter) Write(p []byte) (int, error) {
	rlw.mu.Lock()
	defer rlw.mu.Unlock()

	currentTime := time.Now()
	elapsedTime := currentTime.Sub(rlw.startTime).Milliseconds()
	maxWrittenBytes := int64(float64(elapsedTime) * rlw.bytesPerMillisecond)

	byteBudget := maxWrittenBytes - rlw.bytesWritten

	toWrite := int64(len(p))
	if toWrite > byteBudget {
		toWrite = byteBudget
	}

	n, err := rlw.writer.Write(p[:toWrite])
	rlw.bytesWritten += int64(n)
	if err != nil {
		return n, err
	}

	if int64(len(p)) > byteBudget {
		remainingBytes := int64(len(p)) - byteBudget
		msToWait := int64(float64(remainingBytes) / rlw.bytesPerMillisecond)
		if msToWait > 0 {
			time.Sleep(time.Duration(msToWait) * time.Millisecond)
		}

		n2, err := rlw.writer.Write(p[toWrite:])
		rlw.bytesWritten += int64(n2)
		n += n2
		if err != nil {
			return n, err
		}
	}

	// Reset startTime and bytesWritten to avoid overflow and maintain rate limiting accuracy
	if elapsedTime > 1000000000 {
		rlw.startTime = time.Now()
		rlw.bytesWritten = 0
	}

	return n, nil
}

