//go:build ruleguard

// Managed by httpgen. Changes are overwritten by 'go tool httpgen sync'.

// Package gorules holds the custom lint rules that golangci-lint runs
// through ruleguard. The build tag keeps it out of the service build:
// only golangci-lint reads this file.
package gorules

import "github.com/quasilyte/go-ruleguard/dsl"

// ErrorsAtErrorLevel requires that an error being logged is logged at error
// level, not at debug, info or warn. It finds the error by its type as the
// last argument, and anywhere in the call under the key "err".
//
// The key anchors the patterns in the middle of a call: a pattern with
// arguments on both sides of $err binds it to the first argument and does
// not try the others, so a type alone cannot find it there.
func ErrorsAtErrorLevel(m dsl.Matcher) {
	m.Match(
		`$log.Debug($*_, $err)`,
		`$log.Info($*_, $err)`,
		`$log.Warn($*_, $err)`,
		`$log.DebugContext($*_, $err)`,
		`$log.InfoContext($*_, $err)`,
		`$log.WarnContext($*_, $err)`,
		`$log.Debug($*_, "err", $err, $*_)`,
		`$log.Info($*_, "err", $err, $*_)`,
		`$log.Warn($*_, "err", $err, $*_)`,
		`$log.DebugContext($*_, "err", $err, $*_)`,
		`$log.InfoContext($*_, "err", $err, $*_)`,
		`$log.WarnContext($*_, "err", $err, $*_)`,
		`$log.Debug($*_, slog.Any($k, $err))`,
		`$log.Info($*_, slog.Any($k, $err))`,
		`$log.Warn($*_, slog.Any($k, $err))`,
		`$log.DebugContext($*_, slog.Any($k, $err))`,
		`$log.InfoContext($*_, slog.Any($k, $err))`,
		`$log.WarnContext($*_, slog.Any($k, $err))`,
		`$log.Debug($*_, slog.Any("err", $err), $*_)`,
		`$log.Info($*_, slog.Any("err", $err), $*_)`,
		`$log.Warn($*_, slog.Any("err", $err), $*_)`,
		`$log.DebugContext($*_, slog.Any("err", $err), $*_)`,
		`$log.InfoContext($*_, slog.Any("err", $err), $*_)`,
		`$log.WarnContext($*_, slog.Any("err", $err), $*_)`,
	).
		Where(m["err"].Type.Implements("error")).
		Report(`errors should be logged at error level: use Error`)
}

// ErrlogAtErrorLevel requires the same of an error described by
// errlog.Value, wherever it is in the call: errlog.Value hands slog a
// description rather than the error, so ErrorsAtErrorLevel does not see it.
func ErrlogAtErrorLevel(m dsl.Matcher) {
	m.Match(
		`$log.Debug($*_, errlog.Value($_), $*_)`,
		`$log.Info($*_, errlog.Value($_), $*_)`,
		`$log.Warn($*_, errlog.Value($_), $*_)`,
		`$log.DebugContext($*_, errlog.Value($_), $*_)`,
		`$log.InfoContext($*_, errlog.Value($_), $*_)`,
		`$log.WarnContext($*_, errlog.Value($_), $*_)`,
		`$log.Debug($*_, slog.Any($_, errlog.Value($_)), $*_)`,
		`$log.Info($*_, slog.Any($_, errlog.Value($_)), $*_)`,
		`$log.Warn($*_, slog.Any($_, errlog.Value($_)), $*_)`,
		`$log.DebugContext($*_, slog.Any($_, errlog.Value($_)), $*_)`,
		`$log.InfoContext($*_, slog.Any($_, errlog.Value($_)), $*_)`,
		`$log.WarnContext($*_, slog.Any($_, errlog.Value($_)), $*_)`,
	).
		Report(`errors should be logged at error level: use Error`)
}

// ErrorTextAtErrorLevel requires the same of an error's text: err.Error()
// belongs in an Error call, not in Debug, Info or Warn. It looks in the same
// places as ErrorsAtErrorLevel.
func ErrorTextAtErrorLevel(m dsl.Matcher) {
	m.Match(
		`$log.Debug($*_, $err.Error())`,
		`$log.Info($*_, $err.Error())`,
		`$log.Warn($*_, $err.Error())`,
		`$log.DebugContext($*_, $err.Error())`,
		`$log.InfoContext($*_, $err.Error())`,
		`$log.WarnContext($*_, $err.Error())`,
		`$log.Debug($*_, "err", $err.Error(), $*_)`,
		`$log.Info($*_, "err", $err.Error(), $*_)`,
		`$log.Warn($*_, "err", $err.Error(), $*_)`,
		`$log.DebugContext($*_, "err", $err.Error(), $*_)`,
		`$log.InfoContext($*_, "err", $err.Error(), $*_)`,
		`$log.WarnContext($*_, "err", $err.Error(), $*_)`,
		`$log.Debug($*_, slog.String($k, $err.Error()))`,
		`$log.Info($*_, slog.String($k, $err.Error()))`,
		`$log.Warn($*_, slog.String($k, $err.Error()))`,
		`$log.DebugContext($*_, slog.String($k, $err.Error()))`,
		`$log.InfoContext($*_, slog.String($k, $err.Error()))`,
		`$log.WarnContext($*_, slog.String($k, $err.Error()))`,
		`$log.Debug($*_, slog.String("err", $err.Error()), $*_)`,
		`$log.Info($*_, slog.String("err", $err.Error()), $*_)`,
		`$log.Warn($*_, slog.String("err", $err.Error()), $*_)`,
		`$log.DebugContext($*_, slog.String("err", $err.Error()), $*_)`,
		`$log.InfoContext($*_, slog.String("err", $err.Error()), $*_)`,
		`$log.WarnContext($*_, slog.String("err", $err.Error()), $*_)`,
	).
		Where(m["err"].Type.Implements("error")).
		Report(`errors should be logged at error level: use Error`)
}
