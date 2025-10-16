// Package output contains audio output backends, including an HTTP backend for streaming PCM over HTTP.
package output

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"

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

	writer io.Writer
	mux    *http.ServeMux
	srv    *http.Server

	err chan error
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
		err:            make(chan error, 2),
	}
	out.cond = sync.NewCond(&out.lock)

	// HTTP server with private mux
	out.mux = http.NewServeMux()
	out.mux.HandleFunc("/", out.streamHandler)
	out.srv = &http.Server{Addr: opts.HttpAddress, Handler: out.mux}

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

func (out *httpOutput) outputLoop() {
	floats := make([]float32, 4*1024)
	bytes := make([]byte, len(floats)*2) // s16le

	for {
		out.lock.Lock()

		for out.paused && !out.closed {
			out.cond.Wait()
		}
		if out.closed {
			out.lock.Unlock()
			break
		}

		n, err := out.reader.Read(floats)

		// Apply volume if not external
		if !out.externalVolume {
			volume := out.volume * out.volume
			for i := 0; i < n; i++ {
				floats[i] *= volume
			}
		}

		if n > 0 && out.writer != nil {
			// Convert to big endian 16-bit per sample for audio/L16 MIME (network byte order)
			// But some clients expect little endian. We'll use big endian as per RFC for L16 over RTP; for HTTP, document it.
			for i := 0; i < n; i++ {
				// clamp
				f := floats[i]
				if f > 1 {
					f = 1
				} else if f < -1 {
					f = -1
				}
				val := int16(f * 32767)
				binary.BigEndian.PutUint16(bytes[i*2:], uint16(val))
			}
			_, werr := out.writer.Write(bytes[:n*2])
			if werr != nil {
				out.err <- werr
				out.writer = nil
			}
		}

		if errors.Is(err, io.EOF) {
			out.paused = true
		} else if err != nil {
			out.err <- err
			out.closed = true
			out.lock.Unlock()
			break
		}

		out.lock.Unlock()
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

	out.lock.Lock()
	out.writer = rw
	out.cond.Signal()
	out.lock.Unlock()

	<-r.Context().Done()

	out.lock.Lock()
	out.writer = nil
	out.lock.Unlock()
}

func (out *httpOutput) Pause() error {
	out.lock.Lock()
	defer out.lock.Unlock()
	if out.closed {
		return nil
	}
	out.paused = true
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

func (out *httpOutput) Drop() error { return nil }

func (out *httpOutput) DelayMs() (int64, error) { return 0, nil }

func (out *httpOutput) SetVolume(vol float32) {
	if vol < 0 || vol > 1 {
		panic(fmt.Sprintf("invalid volume value: %0.2f", vol))
	}
	out.volume = vol
}

func (out *httpOutput) Error() <-chan error { return out.err }

func (out *httpOutput) Close() error {
	out.lock.Lock()
	defer out.lock.Unlock()
	if out.closed {
		return nil
	}
	out.closed = true
	out.cond.Signal()
	// Shutdown server gracefully
	if out.srv != nil {
		_ = out.srv.Close()
	}
	return nil
}
