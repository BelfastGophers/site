// Managed by httpgen. Changes are overwritten by 'go tool httpgen sync'.

// Package errlog describes errors for the logs, one way everywhere: every
// error the service logs is under "err", with the same shape, so a log store
// can index the field and a query on it finds them all. It imports nothing
// of the service's, so any package can use it.
package errlog

import (
	"fmt"
	"log/slog"
	"reflect"
	"strings"
)

// maxTypes caps how much of an error chain Value records.
const maxTypes = 16

// Value describes err for a log record. Log it under "err":
//
//	logger.ErrorContext(ctx, "payment failed", "order", id, "err", errlog.Value(err))
//
// The record has three fields. message is the error's text. types lists the
// type of each error in the chain that %w wrapping hides, outermost first,
// following errors.Join as well, up to 16: it shows what failed underneath
// when the text alone does not, such as a syscall.Errno. root is the text of
// the innermost error, the failure itself, when the chain is a single line of
// wrapping, so that failures with the same cause can be grouped.
//
// The description is worked out only when the record is written, so a
// record below the logger's level costs almost nothing.
func Value(err error) slog.LogValuer { return description{err} }

type description struct{ err error }

func (d description) LogValue() slog.Value {
	if d.err == nil {
		return slog.AnyValue(nil)
	}
	var types []string
	linear := true
	innermost := d.err
	var walk func(error)
	walk = func(e error) {
		for e != nil && len(types) < maxTypes {
			types = append(types, fmt.Sprintf("%T", e))
			innermost = e
			inner, joined := unwrap(e)
			if !joined && len(inner) == 1 && inner[0] != nil {
				e = inner[0]
				continue
			}
			// errors.Join and friends hold several causes at once.
			if joined {
				linear = false
				for _, next := range inner {
					walk(next)
				}
			}
			return
		}
		// Capped before the end, so the innermost error was not reached.
		linear = false
	}
	walk(d.err)

	attrs := []slog.Attr{
		slog.String("message", text(d.err)),
		slog.String("types", strings.Join(types, " ")),
	}
	// Counted rather than compared: comparing errors panics when one's
	// type is not comparable.
	if linear && len(types) > 1 {
		attrs = append(attrs, slog.String("root", text(innermost)))
	}
	return slog.GroupValue(attrs...)
}

// unwrap returns what e wraps, and whether it holds several errors at once,
// as errors.Join's does. An Unwrap method that panics, as one on a nil
// pointer usually does, makes e the end of the chain rather than losing the
// whole description.
func unwrap(e error) (inner []error, joined bool) {
	defer func() {
		if recover() != nil {
			inner, joined = nil, false
		}
	}()
	if u, ok := e.(interface{ Unwrap() error }); ok {
		return []error{u.Unwrap()}, false
	}
	if u, ok := e.(interface{ Unwrap() []error }); ok {
		return u.Unwrap(), true
	}
	return nil, false
}

// text returns e's message. Like slog itself, it does not let a nil pointer
// with an Error method panic in the middle of logging: wrapped with %w, one
// is still a non-nil error.
func text(e error) (s string) {
	defer func() {
		if r := recover(); r != nil {
			if v := reflect.ValueOf(e); v.Kind() == reflect.Pointer && v.IsNil() {
				s = "<nil>"
				return
			}
			s = fmt.Sprintf("!PANIC in Error: %v", r)
		}
	}()
	return e.Error()
}
