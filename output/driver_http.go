package output

import (
	"errors"
	"fmt"
	librespot "go-librespot"
	"io"
	"net/http"
	"sync"
)

type httpOutput struct {
	channels   int
	sampleRate int
	address    string
	reader     librespot.Float32Reader

	cond   *sync.Cond
	volume float32
	paused bool
	closed bool
	err    chan error

	writer io.Writer
}

func newHTTPOutput(reader librespot.Float32Reader, sampleRate int, channels int, address string, initialVolume float32) (*httpOutput, error) {
	out := &httpOutput{
		reader:     reader,
		channels:   channels,
		sampleRate: sampleRate,
		address:    address,
		volume:     initialVolume,
		err:        make(chan error, 1),
		cond:       sync.NewCond(&sync.Mutex{}),
	}

	go func() {
		out.err <- out.loop()
		_ = out.Close()
	}()

	http.HandleFunc("/", out.streamHandler)
	go func() {
		if err := http.ListenAndServe(address, nil); err != nil {
			fmt.Printf("HTTP server error: %v\n", err)
		}
	}()

	return out, nil
}

func (out *httpOutput) loop() error {
	// Read buffer of 128MB
	floats := make([]float32, out.channels*16*1024)

	for {
		sampleCount, err := out.reader.Read(floats)
		if errors.Is(err, io.EOF) || errors.Is(err, librespot.ErrDrainReader) {
			if errors.Is(err, io.EOF) {
				return nil
			}

			out.reader.Drained()
			continue
		} else if err != nil {
			return fmt.Errorf("failed reading source: %w", err)
		}

		if sampleCount%out.channels != 0 {
			return fmt.Errorf("invalid read amount: %d", sampleCount)
		}

		// Adjust volume
		for i := 0; i < sampleCount; i++ {
			floats[i] *= out.volume
		}

		out.cond.L.Lock()
		for !(!out.paused || out.closed) {
			out.cond.Wait()
		}

		if out.closed {
			out.cond.L.Unlock()
			return nil
		}

		// Convert audio to 16bit integer big endian
		data := make([]byte, sampleCount*2)
		for i := 0; i < sampleCount; i++ {
			// Convert [-1,1] float by multiplying by max 16bit integer
			intSample := int16(floats[i] * 32767.0)
			data[2*i] = byte(intSample >> 8) // MSB first
			data[2*i+1] = byte(intSample)    // LSB second
		}

		// Write audio out
		if out.writer != nil {
			_, err := out.writer.Write(data)
			if err != nil {
				out.cond.L.Unlock()
				return err
			}
		}

		out.cond.L.Unlock()
	}
}

func (out *httpOutput) streamHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "audio/l16;rate=44100")
	w.Header().Set("Accept-Ranges", "none")

	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}

	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// Let receiver know this is an endless stream
	w.Header().Set("Transfer-Encoding", "chunked")
	w.WriteHeader(http.StatusOK)

	out.cond.L.Lock()
	bytesPerSecond := out.channels * out.sampleRate * 16
	out.writer = NewRateLimitedWriter(w, bytesPerSecond)
	out.cond.Signal()
	out.cond.L.Unlock()

	// Channel to signal the end of streaming
	done := make(chan struct{})

	// Wait for the client to disconnect
	<-r.Context().Done()

	// Signal the end of streaming
	close(done)

	out.cond.L.Lock()
	out.writer = nil
	out.cond.L.Unlock()
}

func (out *httpOutput) Pause() error {
	out.cond.L.Lock()
	defer out.cond.L.Unlock()

	if out.closed || out.paused {
		return nil
	}

	out.paused = true
	return nil
}

func (out *httpOutput) Resume() error {
	out.cond.L.Lock()
	defer out.cond.L.Unlock()

	if out.closed || !out.paused {
		return nil
	}

	out.paused = false
	out.cond.Signal()
	return nil
}

func (out *httpOutput) Drop() error {
	return nil // Not applicable for HTTP
}

func (out *httpOutput) DelayMs() (int64, error) {
	return 0, nil // Not applicable for HTTP
}

func (out *httpOutput) SetVolume(vol float32) {
	if vol < 0 || vol > 1 {
		panic(fmt.Sprintf("invalid volume value: %0.2f", vol))
	}

	out.volume = vol
}

func (out *httpOutput) Error() <-chan error {
	out.cond.L.Lock()
	defer out.cond.L.Unlock()

	return out.err
}

func (out *httpOutput) Close() error {
	out.cond.L.Lock()
	defer out.cond.L.Unlock()

	if out.closed {
		return nil
	}

	out.closed = true
	out.cond.Signal()

	return nil
}
