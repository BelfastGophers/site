// Managed by httpgen. Changes are overwritten by 'go tool httpgen sync'.

// Package logging builds the service's logger. Errors are described for it
// by package errlog.
package logging

import (
	"fmt"
	"io"
	"log/slog"

	"belfastgophers.com/site/api/rest/middleware"
)

// New builds the logger for the whole service, writing to w at level, which
// is debug, info, warn or error. The format is json, the default for log
// collectors, or text, which suits a terminal. Records logged with a
// request's context carry its request id and trace.
func New(w io.Writer, level, format string) (*slog.Logger, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("log level %q: %w", level, err)
	}
	opts := &slog.HandlerOptions{Level: lvl}

	var h slog.Handler
	switch format {
	case "text":
		h = slog.NewTextHandler(w, opts)
	case "json":
		h = slog.NewJSONHandler(w, opts)
	default:
		return nil, fmt.Errorf("unknown log format %q: use text or json", format)
	}
	return slog.New(middleware.LogHandler(h)), nil
}
