package telemetry_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kramersean/k8s-buddy/internal/telemetry"
)

// decodeLines unmarshals each non-empty line of buf as a JSON object,
// returning them in order.
func decodeLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()

	var lines []map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if raw == "" {
			continue
		}
		var line map[string]any
		require.NoError(t, json.Unmarshal([]byte(raw), &line))
		lines = append(lines, line)
	}
	return lines
}

func TestNewLogger_EmitsRenamedKeys(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := telemetry.NewLogger(slog.LevelInfo, &buf)

	logger.Info("plant checked", "mood", "leafy")

	lines := decodeLines(t, &buf)
	require.Len(t, lines, 1)
	line := lines[0]

	require.Contains(t, line, "ts")
	require.Contains(t, line, "level")
	require.Contains(t, line, "msg")
	require.NotContains(t, line, "time", "the default slog time key must be renamed away, not duplicated")

	require.Equal(t, "plant checked", line["msg"])
	require.Equal(t, "INFO", line["level"])
	require.Equal(t, "leafy", line["mood"])
}

func TestNewLogger_LevelFilterSuppressesDebug(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := telemetry.NewLogger(slog.LevelInfo, &buf)

	logger.Debug("should not appear")
	logger.Info("should appear")

	lines := decodeLines(t, &buf)
	require.Len(t, lines, 1, "only the Info line should have been emitted")
	require.Equal(t, "should appear", lines[0]["msg"])
}

func TestNewLogger_DebugLevelAllowsDebug(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := telemetry.NewLogger(slog.LevelDebug, &buf)

	logger.Debug("visible at debug level")

	lines := decodeLines(t, &buf)
	require.Len(t, lines, 1)
	require.Equal(t, "DEBUG", lines[0]["level"])
	require.Equal(t, "visible at debug level", lines[0]["msg"])
}

func TestNewLogger_NestedGroupKeysUntouched(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := telemetry.NewLogger(slog.LevelInfo, &buf)

	// A nested attribute named "time" inside a user group must survive
	// unmangled: ReplaceAttr must only rename the handler's own
	// top-level time key, not any attribute that happens to share its
	// name inside a group.
	logger.Info("request handled",
		slog.Group("request", slog.String("time", "arbitrary-value")),
	)

	lines := decodeLines(t, &buf)
	require.Len(t, lines, 1)

	group, ok := lines[0]["request"].(map[string]any)
	require.True(t, ok, "request group must decode as a nested object")
	require.Equal(t, "arbitrary-value", group["time"])
}
