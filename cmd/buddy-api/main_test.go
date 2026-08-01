package main

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/kramersean/k8s-buddy/internal/api"
	"github.com/kramersean/k8s-buddy/internal/telemetry"
)

// isReady is a small test helper that hits /readyz through the server's
// real Handler and reports whether it returned 200.
func isReady(s *api.Server) bool {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	s.Handler().ServeHTTP(rec, req)
	return rec.Code == http.StatusOK
}

// TestGracefulShutdown_OrderingAndDelay is the regression guard for
// buddy-api's centerpiece behavior: the shutdown sequence must (1) flip
// readiness false before sleeping -- not after -- (2) actually wait out
// the full configured delay before touching the listener, and (3) only
// then call http.Server.Shutdown. gracefulShutdown is a plain
// package-level function taking the delay as a parameter, so none of
// this requires sending a real OS signal or binding a real port under
// contention -- it needs no more than what's set up below. This test
// must fail if phases 2 and 3 are ever reordered, or if the delay is
// dropped.
func TestGracefulShutdown_OrderingAndDelay(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := telemetry.NewMetrics(reg, telemetry.BuildInfo{Version: "t", Commit: "t", GoVersion: "t"})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	srv := api.New(api.Config{}, logger, m, reg)
	srv.SetReady(true)
	require.True(t, isReady(srv), "precondition: server must start ready")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	httpServer := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}

	// RegisterOnShutdown's callback runs the instant httpServer.Shutdown
	// is invoked -- before it waits for connections to drain -- so timing
	// it directly proves *when* Shutdown was called, not just that the
	// overall function eventually returned.
	var mu sync.Mutex
	var shutdownCalledAt time.Time
	httpServer.RegisterOnShutdown(func() {
		mu.Lock()
		shutdownCalledAt = time.Now()
		mu.Unlock()
	})

	serveDone := make(chan error, 1)
	go func() {
		serveDone <- httpServer.Serve(ln)
	}()

	const delay = 300 * time.Millisecond
	start := time.Now()

	// Sample readiness partway through the delay window (well before it
	// elapses) in a separate goroutine, concurrently with the
	// gracefulShutdown call below.
	readyMidSleep := make(chan bool, 1)
	go func() {
		time.Sleep(50 * time.Millisecond)
		readyMidSleep <- isReady(srv)
	}()

	err = gracefulShutdown(httpServer, srv, logger, delay)
	require.NoError(t, err)
	elapsed := time.Since(start)

	require.False(t, <-readyMidSleep,
		"readiness must already be false ~50ms into a 300ms shutdown delay -- "+
			"SetReady(false) must run BEFORE the sleep, not after it")

	require.GreaterOrEqual(t, elapsed, delay,
		"gracefulShutdown must not return before the configured delay has fully elapsed")

	mu.Lock()
	got := shutdownCalledAt
	mu.Unlock()
	require.False(t, got.IsZero(), "http.Server.Shutdown must have been called")
	require.GreaterOrEqual(t, got.Sub(start), delay,
		"http.Server.Shutdown must not be invoked until after the configured delay has elapsed -- "+
			"this is what fails if phases 2 and 3 are ever reordered")

	select {
	case serveErr := <-serveDone:
		require.ErrorIs(t, serveErr, http.ErrServerClosed,
			"Serve must have returned because Shutdown actually ran")
	case <-time.After(2 * time.Second):
		t.Fatal("http.Server.Serve did not return after gracefulShutdown completed")
	}

	require.False(t, isReady(srv), "readiness must remain false after shutdown completes")
}

// TestLoadConfig_Defaults pins the exact shipped default values loadConfig
// falls back to with a clean environment -- including the 150ms
// BUDDY_LATENCY_BUDGET default and the reachability property it exists to
// guarantee (see TestWork_ShippedDefaults_WarningIsReachable in
// internal/api for the corresponding behavioral guard).
func TestLoadConfig_Defaults(t *testing.T) {
	for _, key := range []string{
		"BUDDY_NAME", "BUDDY_SPECIES", "BUDDY_PORT", "BUDDY_LOG_LEVEL",
		"BUDDY_LATENCY_BUDGET", "BUDDY_WORK_ERROR_RATE", "BUDDY_WORK_MIN_DELAY",
		"BUDDY_WORK_MAX_DELAY", "BUDDY_ENABLE_CHAOS_ENDPOINTS", "BUDDY_SHUTDOWN_DELAY",
	} {
		t.Setenv(key, "")
	}

	cfg, err := loadConfig()
	require.NoError(t, err)

	require.Equal(t, "fernie", cfg.plantName)
	require.Equal(t, "fern", cfg.species)
	require.Equal(t, 8080, cfg.port)
	require.Equal(t, "info", cfg.logLevel)
	require.Equal(t, 150*time.Millisecond, cfg.latencyBudget)
	require.InDelta(t, 0.05, cfg.workErrorRate, 0.0001)
	require.Equal(t, 10*time.Millisecond, cfg.workMinDelay)
	require.Equal(t, 200*time.Millisecond, cfg.workMaxDelay)
	require.False(t, cfg.enableChaosEndpoints)
	require.Equal(t, 5*time.Second, cfg.shutdownDelay)

	require.Less(t, cfg.latencyBudget, cfg.workMaxDelay,
		"the shipped latency budget must be below the shipped max work delay, "+
			"or /work's warning outcome is mathematically unreachable under default configuration")
}

// TestLoadConfig_RejectsInvalidValues is table-driven per invalid
// environment value, asserting loadConfig fails with a clear,
// variable-naming error rather than silently substituting a default.
func TestLoadConfig_RejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name       string
		env        map[string]string
		wantErrHas string
	}{
		{
			name:       "non-integer port",
			env:        map[string]string{"BUDDY_PORT": "not-a-port"},
			wantErrHas: "BUDDY_PORT",
		},
		{
			name:       "port out of range",
			env:        map[string]string{"BUDDY_PORT": "70000"},
			wantErrHas: "BUDDY_PORT",
		},
		{
			name:       "zero latency budget",
			env:        map[string]string{"BUDDY_LATENCY_BUDGET": "0s"},
			wantErrHas: "BUDDY_LATENCY_BUDGET",
		},
		{
			name:       "negative latency budget",
			env:        map[string]string{"BUDDY_LATENCY_BUDGET": "-1s"},
			wantErrHas: "BUDDY_LATENCY_BUDGET",
		},
		{
			name:       "malformed latency budget",
			env:        map[string]string{"BUDDY_LATENCY_BUDGET": "not-a-duration"},
			wantErrHas: "BUDDY_LATENCY_BUDGET",
		},
		{
			name:       "work error rate below zero",
			env:        map[string]string{"BUDDY_WORK_ERROR_RATE": "-0.1"},
			wantErrHas: "BUDDY_WORK_ERROR_RATE",
		},
		{
			name:       "work error rate above one",
			env:        map[string]string{"BUDDY_WORK_ERROR_RATE": "1.5"},
			wantErrHas: "BUDDY_WORK_ERROR_RATE",
		},
		{
			name:       "negative work min delay",
			env:        map[string]string{"BUDDY_WORK_MIN_DELAY": "-5ms"},
			wantErrHas: "BUDDY_WORK_MIN_DELAY",
		},
		{
			name:       "negative work max delay",
			env:        map[string]string{"BUDDY_WORK_MAX_DELAY": "-5ms"},
			wantErrHas: "BUDDY_WORK_MAX_DELAY",
		},
		{
			name: "min delay above max delay",
			env: map[string]string{
				"BUDDY_WORK_MIN_DELAY": "500ms",
				"BUDDY_WORK_MAX_DELAY": "100ms",
			},
			wantErrHas: "BUDDY_WORK_MIN_DELAY",
		},
		{
			name:       "invalid chaos bool",
			env:        map[string]string{"BUDDY_ENABLE_CHAOS_ENDPOINTS": "maybe"},
			wantErrHas: "BUDDY_ENABLE_CHAOS_ENDPOINTS",
		},
		{
			name:       "negative shutdown delay",
			env:        map[string]string{"BUDDY_SHUTDOWN_DELAY": "-1s"},
			wantErrHas: "BUDDY_SHUTDOWN_DELAY",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, key := range []string{
				"BUDDY_PORT", "BUDDY_LATENCY_BUDGET", "BUDDY_WORK_ERROR_RATE",
				"BUDDY_WORK_MIN_DELAY", "BUDDY_WORK_MAX_DELAY",
				"BUDDY_ENABLE_CHAOS_ENDPOINTS", "BUDDY_SHUTDOWN_DELAY",
			} {
				t.Setenv(key, "")
			}
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			_, err := loadConfig()
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.wantErrHas)
		})
	}
}
