// Managed by httpgen. Changes are overwritten by 'go tool httpgen sync'.

package errlog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// record is what Value writes under "err".
type record struct {
	Message, Types, Root string
}

// logged logs err through Value and returns what was recorded.
func logged(t *testing.T, err error) record {
	t.Helper()
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Error("failed", "err", Value(err))
	var rec struct{ Err record }
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("%v: %s", err, buf.String())
	}
	return rec.Err
}

func TestValue(t *testing.T) {
	t.Parallel()
	root := errors.New("refused")
	tests := []struct {
		name string
		err  error
		want record
	}{
		{"unwrapped", root, record{"refused", "*errors.errorString", ""}},
		{"wrapped", fmt.Errorf("dial: %w", root), record{"dial: refused", "*fmt.wrapError *errors.errorString", "refused"}},
		{
			"joined", errors.Join(root, errors.New("timeout")),
			// A join has no single root: the message holds every cause.
			record{"refused\ntimeout", "*errors.joinError *errors.errorString *errors.errorString", ""},
		},
		{"nil pointer", fmt.Errorf("op: %w", (*nilError)(nil)), record{"op: <nil>", "*fmt.wrapError *errlog.nilError", "<nil>"}},
		{
			"nil pointer that unwraps", fmt.Errorf("op: %w", (*queryError)(nil)),
			// Its Unwrap panics too, so the chain ends there.
			record{"op: <nil>", "*fmt.wrapError *errlog.queryError", "<nil>"},
		},
		{"panicking Error", boomError{}, record{"!PANIC in Error: boom", "errlog.boomError", ""}},
	}
	for _, tt := range tests {
		if got := logged(t, tt.err); got != tt.want {
			t.Errorf("%s: logged %+v, want %+v", tt.name, got, tt.want)
		}
	}

	deep := root
	for range 2 * maxTypes {
		deep = fmt.Errorf("again: %w", deep)
	}
	got := logged(t, deep)
	if types := strings.Fields(got.Types); len(types) != maxTypes {
		t.Errorf("deep: %d types, want them capped at %d", len(types), maxTypes)
	}
	if got.Root != "" {
		t.Errorf("deep: root = %q, want none: the chain was cut before its end", got.Root)
	}
}

// nilError has an Error method that panics on a nil pointer, as most do.
type nilError struct{ msg string }

func (e *nilError) Error() string { return e.msg }

// queryError wraps another error, and its Unwrap panics on a nil pointer as
// its Error does.
type queryError struct{ err error }

func (e *queryError) Error() string { return "query: " + e.err.Error() }
func (e *queryError) Unwrap() error { return e.err }

// boomError's Error panics whatever the receiver.
type boomError struct{}

func (boomError) Error() string { panic("boom") }

// countingError counts the calls to its Error method.
type countingError struct{ calls *int }

func (e countingError) Error() string {
	*e.calls++
	return "counted"
}

// TestValueLazy checks that a record the logger drops does no work: the
// description is worked out only when a record is written.
func TestValueLazy(t *testing.T) {
	t.Parallel()
	var calls int
	err := countingError{&calls}
	var buf bytes.Buffer
	quiet := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError + 1}))
	quiet.Error("dropped", "err", Value(err))
	if calls != 0 {
		t.Errorf("Error called %d times for a dropped record, want 0", calls)
	}
	slog.New(slog.NewJSONHandler(&buf, nil)).Error("written", "err", Value(err))
	if calls == 0 {
		t.Error("Error never called for a written record")
	}
}

// TestValueNil checks that a nil error logs as nothing rather than
// panicking in the middle of logging something else.
func TestValueNil(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Error("failed", "err", Value(nil))
	if !bytes.Contains(buf.Bytes(), []byte(`"err":null`)) {
		t.Errorf("logged %s, want err null", buf.String())
	}
}

// TestValueText checks that the text format, which make run uses, is as
// readable as JSON.
func TestValueText(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Error("failed", "err", Value(fmt.Errorf("dial: %w", errors.New("refused"))))
	want := `err.message="dial: refused" err.types="*fmt.wrapError *errors.errorString" err.root=refused`
	if !strings.Contains(buf.String(), want) {
		t.Errorf("logged %s, want it to contain %s", buf.String(), want)
	}
}
