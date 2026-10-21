// Managed by httpgen. Changes are overwritten by 'go tool httpgen sync'.

package writer

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestStarted checks each way a response can start, or seem to and not.
func TestStarted(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		do      func(http.ResponseWriter)
		hide    bool // hide the recorder's Flush, as http.TimeoutHandler does
		started bool
		status  int
	}{
		{"nothing", func(http.ResponseWriter) {}, false, false, http.StatusOK},
		{"final status", func(w http.ResponseWriter) { w.WriteHeader(http.StatusCreated) }, false, true, http.StatusCreated},
		{"103 Early Hints", func(w http.ResponseWriter) { w.WriteHeader(http.StatusEarlyHints) }, false, false, http.StatusOK},
		{"101 Switching Protocols", func(w http.ResponseWriter) { w.WriteHeader(http.StatusSwitchingProtocols) }, false, true, http.StatusSwitchingProtocols},
		{"second status ignored", func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusCreated)
			w.WriteHeader(http.StatusInternalServerError)
		}, false, true, http.StatusCreated},
		{"write", func(w http.ResponseWriter) { _, _ = io.WriteString(w, "x") }, false, true, http.StatusOK},
		{"flush", func(w http.ResponseWriter) { _ = http.NewResponseController(w).Flush() }, false, true, http.StatusOK},
		{"http.Flusher", func(w http.ResponseWriter) { w.(http.Flusher).Flush() }, false, true, http.StatusOK},
		{"failed flush", func(w http.ResponseWriter) { _ = http.NewResponseController(w).Flush() }, true, false, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var under http.ResponseWriter = httptest.NewRecorder()
			if tt.hide {
				under = struct{ http.ResponseWriter }{under}
			}
			w := Wrap(under)
			tt.do(w)
			if w.Started() != tt.started || w.Status() != tt.status {
				t.Errorf("started = %v, status = %d; want %v, %d", w.Started(), w.Status(), tt.started, tt.status)
			}
		})
	}
}

func TestBytes(t *testing.T) {
	t.Parallel()
	w := Wrap(httptest.NewRecorder())
	_, _ = io.WriteString(w, "hello")
	_, _ = io.WriteString(w, ", world")
	if w.Bytes() != 12 {
		t.Errorf("bytes = %d, want 12", w.Bytes())
	}
}

// TestWrapReuses checks that layers share one Writer, so the router sees
// what the middleware's Writer saw.
func TestWrapReuses(t *testing.T) {
	t.Parallel()
	outer := Wrap(httptest.NewRecorder())
	if Wrap(outer) != outer {
		t.Error("Wrap made a second Writer around one")
	}
}

// TestHijack checks that taking the connection over starts the response.
func TestHijack(t *testing.T) {
	t.Parallel()
	got := make(chan *Writer, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		w := Wrap(rw)
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Error(err)
		} else {
			_ = conn.Close()
		}
		got <- w
	}))
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := srv.Client().Do(req); err == nil {
		_ = resp.Body.Close()
	}
	if w := <-got; !w.Started() || !w.Hijacked() {
		t.Errorf("started = %v, hijacked = %v; want both after a hijack", w.Started(), w.Hijacked())
	}
}
