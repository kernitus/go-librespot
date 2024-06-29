package output

import (
	"errors"
	"fmt"
	librespot "go-librespot"
	"io"
	"math"
	"net"
	"sync"
)

type rtpOutput struct {
	channels   int
	sampleRate int
	address    string
	reader     librespot.Float32Reader

	conn net.Conn
	cond *sync.Cond

	volume float32
	paused bool
	closed bool
	err    chan error
}

func newRTPOutput(reader librespot.Float32Reader, sampleRate int, channels int, address string, initialVolume float32) (*rtpOutput, error) {
	conn, err := net.Dial("udp", address)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to RTP address: %w", err)
	}

	out := &rtpOutput{
		reader:     reader,
		channels:   channels,
		sampleRate: sampleRate,
		address:    address,
		conn:       conn,
		volume:     initialVolume,
		err:        make(chan error, 1),
		cond:       sync.NewCond(&sync.Mutex{}),
	}

	go func() {
		out.err <- out.loop()
		_ = out.Close()
	}()

	return out, nil
}

func (out *rtpOutput) loop() error {
	const maxPacketSize = 65507 // Max UDP packet size
	floats := make([]float32, out.channels*16*1024)

	for {
		n, err := out.reader.Read(floats)
		if errors.Is(err, io.EOF) || errors.Is(err, librespot.ErrDrainReader) {
			if errors.Is(err, io.EOF) {
				return nil
			}

			out.reader.Drained()
			continue
		} else if err != nil {
			return fmt.Errorf("failed reading source: %w", err)
		}

		if n%out.channels != 0 {
			return fmt.Errorf("invalid read amount: %d", n)
		}

		for i := 0; i < n; i++ {
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

		data := make([]byte, n*4)
		for i, f := range floats[:n] {
			bits := math.Float32bits(f)
			data[4*i] = byte(bits)
			data[4*i+1] = byte(bits >> 8)
			data[4*i+2] = byte(bits >> 16)
			data[4*i+3] = byte(bits >> 24)
		}

		// Send data in chunks
		for start := 0; start < len(data); start += maxPacketSize {
			end := start + maxPacketSize
			if end > len(data) {
				end = len(data)
			}

			_, err = out.conn.Write(data[start:end])
			if err != nil {
				out.cond.L.Unlock()
				return fmt.Errorf("failed to send data: %w", err)
			}
		}

		out.cond.L.Unlock()
	}
}

func (out *rtpOutput) Pause() error {
	out.cond.L.Lock()
	defer out.cond.L.Unlock()

	if out.closed || out.paused {
		return nil
	}

	out.paused = true
	return nil
}

func (out *rtpOutput) Resume() error {
	out.cond.L.Lock()
	defer out.cond.L.Unlock()

	if out.closed || !out.paused {
		return nil
	}

	out.paused = false
	out.cond.Signal()
	return nil
}

func (out *rtpOutput) Drop() error {
	return nil // Not applicable for RTP
}

func (out *rtpOutput) DelayMs() (int64, error) {
	return 0, nil // Not applicable for RTP
}

func (out *rtpOutput) SetVolume(vol float32) {
	if vol < 0 || vol > 1 {
		panic(fmt.Sprintf("invalid volume value: %0.2f", vol))
	}

	out.volume = vol
}

func (out *rtpOutput) Error() <-chan error {
	out.cond.L.Lock()
	defer out.cond.L.Unlock()

	return out.err
}

func (out *rtpOutput) Close() error {
	out.cond.L.Lock()
	defer out.cond.L.Unlock()

	if out.closed {
		return nil
	}

	out.closed = true
	out.conn.Close()
	out.cond.Signal()

	return nil
}
