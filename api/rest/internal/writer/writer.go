// Managed by httpgen. Changes are overwritten by 'go tool httpgen sync'.

// Package writer records what a handler did with its response: whether the
// response has started, and its status and size. The router and the
// middleware share it, so what counts as started is decided in one place.
package writer

import (
	"bufio"
	"net"
	"net/http"
)

// Writer wraps a response writer and records what passes through it.
type Writer struct {
	http.ResponseWriter
	status   int
	bytes    int64
	started  bool
	hijacked bool
}

// Wrap returns w as a Writer, reusing one an outer layer made, so that a
// request carries a single Writer however many layers need one.
func Wrap(w http.ResponseWriter) *Writer {
	if rw, ok := w.(*Writer); ok {
		return rw
	}
	return &Writer{ResponseWriter: w, status: http.StatusOK}
}

// Started reports whether the response has begun: a final status, a write,
// a successful flush or a hijack. After that the client has a status, and an
// error can no longer change it.
func (w *Writer) Started() bool { return w.started }

// Status returns the final status, 200 if the handler wrote without setting
// one.
func (w *Writer) Status() int { return w.status }

// Bytes returns how many bytes of body went through the Writer.
func (w *Writer) Bytes() int64 { return w.bytes }

// Hijacked reports whether the handler took the connection over, after
// which status and size stop at what went through the Writer.
func (w *Writer) Hijacked() bool { return w.hijacked }

// WriteHeader records the final status. A 1xx is informational, sent ahead
// of it, except 101, which is the last status the client sees. A status after
// the first is ignored by net/http, so it is not recorded either.
func (w *Writer) WriteHeader(status int) {
	final := status >= http.StatusOK || status == http.StatusSwitchingProtocols
	if final && !w.started {
		w.status = status
		w.started = true
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *Writer) Write(b []byte) (int, error) {
	w.started = true
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// FlushError flushes through to the client. A flush sends the header, so it
// starts the response, but only if it succeeds: behind a writer that cannot
// flush, such as http.TimeoutHandler's, nothing was sent.
// http.ResponseController finds this method, and Hijack, before it unwraps.
func (w *Writer) FlushError() error {
	err := http.NewResponseController(w.ResponseWriter).Flush()
	if err == nil {
		w.started = true
	}
	return err
}

// Flush serves code that asserts http.Flusher rather than using
// http.ResponseController.
func (w *Writer) Flush() { _ = w.FlushError() }

// Hijack hands the connection to the handler, as a WebSocket upgrade does.
// The handler then writes whatever goes on it, so a hijack starts the
// response.
func (w *Writer) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		w.started = true
		w.hijacked = true
	}
	return conn, rw, err
}

// Unwrap lets http.ResponseController reach the underlying writer for
// everything else, such as deadlines.
func (w *Writer) Unwrap() http.ResponseWriter { return w.ResponseWriter }
