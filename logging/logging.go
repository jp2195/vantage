// Package logging builds the one slog.Logger each vantage daemon logs
// through, from the log_level and log_format keys all three configs share.
//
// It lives in one package so the three daemons cannot disagree about what
// the keys accept: a value one daemon takes and another rejects would make
// the same config snippet valid in one file and fatal in the next.
package logging

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
)

// The accepted values. The empty string is also accepted everywhere and
// means the default (info, text), so a config that names neither key, or a
// Config built in code, behaves as documented.
const (
	LevelDebug = "debug"
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"

	FormatText = "text"
	FormatJSON = "json"
)

// Validate reports whether level and format are values New understands.
// Each daemon's config validation calls it, so a typo fails config load
// instead of silently logging at the default level.
//
// Matching is exact and lowercase: "INFO" and "warning" are refused rather
// than folded, the same way every other enumerated config key in this
// project is.
func Validate(level, format string) error {
	var errs []error
	if _, ok := parseLevel(level); !ok {
		errs = append(errs, fmt.Errorf("log_level: %q is not one of %s, %s, %s, %s",
			level, LevelDebug, LevelInfo, LevelWarn, LevelError))
	}
	switch format {
	case "", FormatText, FormatJSON:
	default:
		errs = append(errs, fmt.Errorf("log_format: %q is neither %s nor %s",
			format, FormatText, FormatJSON))
	}
	return errors.Join(errs...)
}

// New returns a logger writing to w at level in format. It expects values
// Validate has accepted; anything else falls back to the default for that
// key rather than panicking, since the config layer is where refusing
// belongs.
func New(level, format string, w io.Writer) *slog.Logger {
	lvl, _ := parseLevel(level)
	opts := &slog.HandlerOptions{Level: lvl}
	if format == FormatJSON {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

func parseLevel(s string) (slog.Level, bool) {
	switch s {
	case "", LevelInfo:
		return slog.LevelInfo, true
	case LevelDebug:
		return slog.LevelDebug, true
	case LevelWarn:
		return slog.LevelWarn, true
	case LevelError:
		return slog.LevelError, true
	}
	return slog.LevelInfo, false
}
