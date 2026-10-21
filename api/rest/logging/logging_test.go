// Managed by httpgen. Changes are overwritten by 'go tool httpgen sync'.

package logging

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestNew(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		level, format string
		want          string // a fragment of the line logged at info
	}{
		{"info", "json", `"msg":"hello"`},
		{"debug", "text", "msg=hello"},
		{"error", "json", ""}, // info is below the level
	} {
		var buf bytes.Buffer
		logger, err := New(&buf, tt.level, tt.format)
		if err != nil {
			t.Fatalf("New(%q, %q): %v", tt.level, tt.format, err)
		}
		logger.Info("hello")
		switch {
		case tt.want == "" && buf.Len() != 0:
			t.Errorf("New(%q, %q) logged %q below its level", tt.level, tt.format, buf.String())
		case !strings.Contains(buf.String(), tt.want):
			t.Errorf("New(%q, %q) logged %q, want %q", tt.level, tt.format, buf.String(), tt.want)
		}
	}

	for _, bad := range [][2]string{{"loud", "json"}, {"info", "xml"}} {
		if _, err := New(io.Discard, bad[0], bad[1]); err == nil {
			t.Errorf("New(%q, %q) accepted a bad setting", bad[0], bad[1])
		}
	}
}
