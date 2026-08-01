package telemetry

import (
	"io"
	"log/slog"
)

// NewLogger returns a *slog.Logger backed by a JSON handler that writes to
// w and is filtered to level and above (slog.LevelDebug < LevelInfo <
// LevelWarn < LevelError). It is the only logger constructor K8s Buddy
// components use, so every log line across the project shares one JSON
// shape.
//
// Two of slog's default attribute keys are normalized via ReplaceAttr:
//   - The time key is renamed from slog's default "time" to "ts".
//   - The message key stays "msg" -- already slog's default -- kept
//     explicit here so a future change to the stdlib default can never
//     silently change K8s Buddy's log format out from under it.
func NewLogger(level slog.Level, w io.Writer) *slog.Logger {
	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:       level,
		ReplaceAttr: replaceAttr,
	})
	return slog.New(handler)
}

// replaceAttr renames slog's built-in time attribute key ("time") to "ts".
// It only touches the top-level time attribute: when groups is non-empty,
// a is a member of a user-created group rather than one of the handler's
// own reserved keys, so it is returned unmodified. Without that check, a
// caller's own nested field happening to be named "time" would be mangled
// too.
func replaceAttr(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 && a.Key == slog.TimeKey {
		a.Key = "ts"
	}
	return a
}
