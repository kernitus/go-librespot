// Package output contains audio output backends, including an HTTP backend for streaming PCM over HTTP.
package output

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"sync"
	"time"

	librespot "github.com/devgianlu/go-librespot"
)

type httpOutput struct {
	reader librespot.Float32Reader

	lock sync.Mutex
	cond *sync.Cond

	externalVolume bool

	volume float32
	paused bool
	closed bool

	sampleRate   int
	channelCount int

	connID               uint64
	writer               io.Writer
	writerConnID         uint64
	writerCloseCh        chan struct{}
	writerCloseConnID    uint64
	lastDisconnectConnID uint64

	connBytes      int64
	connFirstWrite time.Time
	connPromoted   bool
	promotedConnID uint64
	inactiveConnID uint64
	inactiveTimer  *time.Timer

	fadeInTotal int
	fadeInPos   int
	mux         *http.ServeMux
	srv         *http.Server

	err chan error
}

func (out *httpOutput) closeActiveConnLocked() {
	// Stop the current HTTP response handler (if any).
	if out.writerCloseCh != nil {
		ch := out.writerCloseCh
		out.writerCloseCh = nil
		out.writerCloseConnID = 0
		close(ch)
	}
	// Detach writer so the audio loop stops writing.
	out.writer = nil
}

func newHTTPOutput(opts *NewOutputOptions) (*httpOutput, error) {
	if opts.HttpAddress == "" {
		return nil, fmt.Errorf("http backend requires HttpAddress")
	}

	out := &httpOutput{
		reader:         opts.Reader,
		sampleRate:     opts.SampleRate,
		channelCount:   opts.ChannelCount,
		volume:         opts.InitialVolume,
		externalVolume: opts.ExternalVolume,
		err:            make(chan error, 16),
	}
	out.cond = sync.NewCond(&out.lock)

	// HTTP server with private mux
	out.mux = http.NewServeMux()
	out.mux.HandleFunc("/", out.streamHandler)
	out.srv = &http.Server{
		Addr:              opts.HttpAddress,
		Handler:           out.mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		// WriteTimeout must be disabled for infinite streaming responses.
		WriteTimeout: 0,
		// IdleTimeout mainly affects keep-alives; we force Connection: close.
		IdleTimeout: 30 * time.Second,
	}

	// Start audio loop
	go out.outputLoop()

	// Start HTTP server in background; report start errors
	ln, err := net.Listen("tcp", opts.HttpAddress)
	if err != nil {
		return nil, fmt.Errorf("failed starting http output listener: %w", err)
	}
	go func() {
		if serveErr := out.srv.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			out.err <- serveErr
		}
	}()

	return out, nil
}

func (out *httpOutput) Persistent() bool { return true }

func (out *httpOutput) outputLoop() {
	floats := make([]float32, 4*1024)
	bytes := make([]byte, len(floats)*2)

	for {
		out.lock.Lock()
		for (out.paused || out.writer == nil) && !out.closed {
			out.cond.Wait()
		}
		if out.closed {
			out.lock.Unlock()
			break
		}

		writer := out.writer
		writerConnID := out.writerConnID
		externalVolume := out.externalVolume
		volume := out.volume
		fadeInTotal := out.fadeInTotal
		fadeInPos := out.fadeInPos
		out.lock.Unlock()

		n, rerr := out.reader.Read(floats)

		if n > 0 {
			// Apply volume if not external
			if !externalVolume {
				gain := volume * volume
				for i := 0; i < n; i++ {
					floats[i] *= gain
				}
			}

			// Short fade-in after transitions to avoid discontinuity artifacts.
			if fadeInTotal > 0 && fadeInPos < fadeInTotal {
				toFade := n
				remaining := fadeInTotal - fadeInPos
				if toFade > remaining {
					toFade = remaining
				}
				den := float32(fadeInTotal)
				for i := 0; i < toFade; i++ {
					g := float32(fadeInPos+i) / den
					floats[i] *= g
				}
				newFadeInPos := fadeInPos + toFade
				out.lock.Lock()
				if out.fadeInTotal == fadeInTotal && out.fadeInPos == fadeInPos {
					out.fadeInPos = newFadeInPos
				}
				out.lock.Unlock()
			}

			// Convert to big endian 16-bit per sample for audio/L16 MIME.
			for i := 0; i < n; i++ {
				f := floats[i]
				if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) {
					f = 0
				}
				if f > 1 {
					f = 1
				} else if f < -1 {
					f = -1
				}
				val := int16(f * 32767)
				binary.BigEndian.PutUint16(bytes[i*2:], uint16(val))
			}

			written, werr := writer.Write(bytes[:n*2])
			if written > 0 {
				out.lock.Lock()
				if out.writerConnID == writerConnID {
					if out.connFirstWrite.IsZero() {
						out.connFirstWrite = time.Now()
					}
					out.connBytes += int64(written)

					// Promote this connection to the active sink only after it has
					// demonstrably streamed for a bit, to avoid treating Kodi probe
					// connections as real playback.
					if !out.connPromoted && !out.connFirstWrite.IsZero() {
						if out.connBytes >= 256*1024 || time.Since(out.connFirstWrite) >= 500*time.Millisecond {
							out.connPromoted = true
							out.promotedConnID = writerConnID
							// Cancel any pending inactive transition.
							if out.inactiveTimer != nil {
								out.inactiveTimer.Stop()
								out.inactiveTimer = nil
								out.inactiveConnID = 0
							}
						}
					}
				}
				out.lock.Unlock()
			}
			if werr != nil {
				out.lock.Lock()
				// Only act on errors for the currently active connection.
				if out.writerConnID == writerConnID {
					out.writer = nil
					out.cond.Signal()
					out.onConnGoneLocked(writerConnID)
				}
				out.lock.Unlock()
			}
		}

		if errors.Is(rerr, io.EOF) {
			out.lock.Lock()
			out.paused = true
			out.cond.Signal()
			out.lock.Unlock()
		} else if rerr != nil {
			sendErr(out.err, rerr)
			out.lock.Lock()
			out.closed = true
			out.cond.Signal()
			out.lock.Unlock()
			break
		}
	}

	_ = out.Close()
}

func (out *httpOutput) streamHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// Informational headers
	w.Header().Set("Content-Type", fmt.Sprintf("audio/L16;rate=%d;channels=%d", out.sampleRate, out.channelCount))
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Connection", "close")

	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Rate limit stream at sampleRate*channels*2 bytes/sec
	bps := out.sampleRate * out.channelCount * 2
	rw := NewRateLimitedWriter(w, bps)
	closeCh := make(chan struct{})

	out.lock.Lock()
	// Replacing an existing active sink counts as a disconnect of the previous one.
	prevConnID := out.writerConnID
	if out.writer != nil {
		out.closeActiveConnLocked()
		out.cond.Signal()
		out.onConnGoneLocked(prevConnID)
	}
	out.connID++
	connID := out.connID
	out.writer = rw
	out.writerConnID = connID
	out.writerCloseCh = closeCh
	out.writerCloseConnID = connID
	out.connBytes = 0
	out.connFirstWrite = time.Time{}
	out.connPromoted = false
	// Fade in on a new connection to avoid pops.
	out.fadeInTotal = out.sampleRate * out.channelCount / 50
	if out.fadeInTotal < 1 {
		out.fadeInTotal = 1
	}
	out.fadeInPos = 0
	out.cond.Signal()
	out.lock.Unlock()

	select {
	case <-r.Context().Done():
	case <-closeCh:
	}

	out.lock.Lock()
	if out.writerConnID == connID {
		out.writer = nil
		if out.writerCloseConnID == connID {
			out.writerCloseCh = nil
			out.writerCloseConnID = 0
		}
		out.cond.Signal()
		out.onConnGoneLocked(connID)
	}
	out.lock.Unlock()
}

func (out *httpOutput) onConnGoneLocked(connID uint64) {
	// Ignore duplicates.
	if out.lastDisconnectConnID == connID {
		return
	}
	out.lastDisconnectConnID = connID

	// Only consider real sink loss for promoted (non-probe) connections.
	if out.promotedConnID != connID {
		return
	}
	out.promotedConnID = 0
	out.connPromoted = false
	out.connBytes = 0
	out.connFirstWrite = time.Time{}

	// If we're paused, don't treat sink disconnects as device inactivity.
	// Spotify pause should keep the device available.
	if out.paused {
		return
	}

	// Debounce inactivity: Kodi may probe/reopen quickly.
	out.inactiveConnID = connID
	if out.inactiveTimer != nil {
		out.inactiveTimer.Stop()
		out.inactiveTimer = nil
	}
	out.inactiveTimer = time.AfterFunc(1500*time.Millisecond, func() {
		out.lock.Lock()
		defer out.lock.Unlock()
		if out.closed {
			return
		}
		// Only trigger if no new sink has been promoted since.
		if out.promotedConnID != 0 {
			return
		}
		if out.inactiveConnID != connID {
			return
		}
		out.inactiveTimer = nil
		out.inactiveConnID = 0
		sendErr(out.err, ErrSinkDisconnected)
	})
}

func (out *httpOutput) Pause() error {
	out.lock.Lock()
	defer out.lock.Unlock()
	if out.closed {
		return nil
	}
	out.paused = true
	// Pausing should never transition the device to inactive.
	if out.inactiveTimer != nil {
		out.inactiveTimer.Stop()
		out.inactiveTimer = nil
		out.inactiveConnID = 0
	}
	// Force-close the active HTTP stream so Kodi doesn't hang waiting for bytes.
	out.closeActiveConnLocked()
	out.cond.Signal()
	return nil
}

func (out *httpOutput) Resume() error {
	out.lock.Lock()
	defer out.lock.Unlock()
	if out.closed {
		return nil
	}
	out.paused = false
	out.cond.Signal()
	return nil
}

func (out *httpOutput) Drop() error {
	// There's no internal device buffer to flush, but a track/seek transition can
	// briefly produce unpleasant artifacts for network clients. Apply a short
	// fade-in window to smooth the discontinuity.
	out.lock.Lock()
	defer out.lock.Unlock()
	if out.closed {
		return nil
	}

	// ~20ms of audio.
	out.fadeInTotal = out.sampleRate * out.channelCount / 50
	if out.fadeInTotal < 1 {
		out.fadeInTotal = 1
	}
	out.fadeInPos = 0
	return nil
}

func (out *httpOutput) DelayMs() (int64, error) { return 0, nil }

func (out *httpOutput) SetVolume(vol float32) {
	if vol < 0 || vol > 1 {
		panic(fmt.Sprintf("invalid volume value: %0.2f", vol))
	}
	out.lock.Lock()
	out.volume = vol
	out.lock.Unlock()
}

func (out *httpOutput) Error() <-chan error { return out.err }

func (out *httpOutput) Close() error {
	out.lock.Lock()
	if out.closed {
		out.lock.Unlock()
		return nil
	}
	out.closed = true
	out.closeActiveConnLocked()
	out.cond.Signal()
	srv := out.srv
	out.srv = nil
	out.lock.Unlock()

	// Shutdown server gracefully.
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}

	return nil
}
