package output

import "errors"

// ErrSinkDisconnected signals that the active output sink disappeared.
//
// For the HTTP backend this means the client connection ended.
var ErrSinkDisconnected = errors.New("output sink disconnected")

func sendErr(ch chan error, err error) {
	// Best-effort delivery while avoiding deadlocks.
	select {
	case ch <- err:
		return
	default:
	}

	// Drop one old value, then retry.
	select {
	case <-ch:
	default:
	}
	select {
	case ch <- err:
	default:
	}
}
