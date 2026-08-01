package api

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

// contextKey is a private type for context values this package sets, so
// its keys can never collide with a key set by another package using a
// plain string or int (the classic footgun context.WithValue's own docs
// warn about).
type contextKey string

// requestIDContextKey is the context key withRequestLogging stores each
// request's ID under.
const requestIDContextKey contextKey = "requestID"

// statusRecorder wraps http.ResponseWriter to capture the status code a
// handler wrote, since net/http gives no way to ask a ResponseWriter
// after the fact what status it sent. Every middleware below that needs
// the final status wraps whatever ResponseWriter it received with its
// own statusRecorder; nesting several of these is harmless, since each
// layer just delegates Write/WriteHeader to the one inside it and
// records the same status independently.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func newStatusRecorder(w http.ResponseWriter) *statusRecorder {
	return &statusRecorder{ResponseWriter: w, status: http.StatusOK}
}

// WriteHeader records status the first time it's called and forwards
// every call to the underlying ResponseWriter -- including repeats,
// which net/http already handles (by logging "superfluous WriteHeader
// call" and otherwise ignoring them), so there's no need to duplicate
// that guard here beyond not overwriting the recorded status.
func (r *statusRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

// Write implicitly sends a 200 if no header was written yet, exactly
// like the underlying http.ResponseWriter does -- so status stays
// accurate for handlers that never call WriteHeader explicitly.
func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	return r.ResponseWriter.Write(b)
}

// withRecovery converts a panicking handler into a logged 500 instead of
// letting the panic take down the request unanswered. net/http's own
// server already recovers panics per connection so one bad request can't
// crash the whole process, but its default behavior is to abort the
// connection with no response at all beyond a stack trace on stderr --
// fine for a human watching logs, useless for a client or a Kubernetes
// probe waiting on a response. This middleware turns that into a
// well-formed 500 plus a structured log line, so a bug in one handler
// degrades a single request instead of looking like the server vanished.
func (s *Server) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic recovered in HTTP handler",
					"panic", rec,
					"method", r.Method,
					"path", r.URL.Path,
					"stack", string(debug.Stack()),
				)
				s.writeJSON(w, http.StatusInternalServerError, statusBody{Status: "internal error"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// withMetrics records one buddy_http_requests_total observation per
// request, labeled with the matched ROUTE PATTERN -- never the raw
// request path.
//
// Go 1.22+'s http.ServeMux sets Request.Pattern to whichever registered
// pattern matched (e.g. "GET /work") before invoking the handler, and
// leaves it empty when nothing matched; routePattern below extracts the
// path portion of that. Using the raw URL path as a metric label instead
// would let any client mint a brand-new permanent Prometheus time series
// just by requesting a novel path -- /foo, /foo/1, /foo/2, a vulnerability
// scanner's probe strings, anything an attacker or a typo sends -- which
// is exactly the unbounded-cardinality failure mode that takes down a
// metrics backend in production. Every unmatched request instead shares
// one stable placeholder series, so 404-ish traffic is still visible in
// aggregate without the series count ever growing unbounded.
func (s *Server) withMetrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := newStatusRecorder(w)
		next.ServeHTTP(rec, r)
		s.metrics.ObserveHTTP(routePattern(r), r.Method, rec.status)
	})
}

// routePattern extracts the path portion of Request.Pattern. Patterns in
// this package are registered with a method prefix (mux.HandleFunc("GET
// /work", ...)), so r.Pattern after a match is "GET /work", not "/work"
// -- stripping the method here keeps the "path" metric label from
// redundantly repeating what the separate "method" label already says.
// An empty Pattern (no route matched) becomes unmatchedRoutePattern.
func routePattern(r *http.Request) string {
	if r.Pattern == "" {
		return unmatchedRoutePattern
	}
	if _, path, ok := strings.Cut(r.Pattern, " "); ok {
		return path
	}
	return r.Pattern
}

// probeRoutes are the routes the kubelet polls on a fixed schedule rather
// than routes a human or a client ever calls. Requests to them are logged
// at Debug, not Info -- see withRequestLogging for why.
var probeRoutes = map[string]bool{
	"/healthz": true,
	"/readyz":  true,
}

// withRequestLogging assigns a per-request ID, attaches it to the
// response as a header and to the request's context for any downstream
// code that wants it, and emits exactly one structured log line per
// request once it completes. It is the outermost layer in the middleware
// chain (see Handler) specifically so its timer covers metrics recording
// and panic recovery too, giving an accurate end-to-end duration and a
// correct final status even for a request that ended in a recovered
// panic.
//
// Probe requests are logged at Debug; everything else at Info. The probes
// run on a fixed cadence forever (readiness every 2s, liveness every 10s,
// per the Deployment), so on a live pod they are the overwhelming majority
// of all traffic -- a sample from this cluster showed 1385 /readyz and 278
// /healthz lines against 5 /status lines, 99.6% noise. At Info they bury
// the lines that actually carry information, in particular the ordered
// graceful-shutdown phase messages that exist precisely to be READ during
// a rollout. Dropping probes to Debug keeps them available (set
// BUDDY_LOG_LEVEL=debug) without letting a timer-driven heartbeat drown
// out events. Nothing is silenced: every probe is still counted in
// buddy_http_requests_total, which is the right instrument for "how many"
// anyway.
func (s *Server) withRequestLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := newRequestID()
		w.Header().Set("X-Request-Id", id)
		r = r.WithContext(context.WithValue(r.Context(), requestIDContextKey, id))

		rec := newStatusRecorder(w)
		start := time.Now()
		next.ServeHTTP(rec, r)
		duration := time.Since(start)

		route := routePattern(r)
		level := slog.LevelInfo
		if probeRoutes[route] {
			level = slog.LevelDebug
		}

		s.log.Log(r.Context(), level, "http request",
			"requestId", id,
			"method", r.Method,
			"route", route,
			"path", r.URL.Path,
			"status", rec.status,
			"durationMs", duration.Milliseconds(),
		)
	})
}

// newRequestID returns a short random hex request identifier. It draws
// from crypto/rand rather than the package's own math/rand source: that
// source exists solely for /work's simulated delay and outcome sampling
// and is deliberately injectable (Config.Rand) for test determinism --
// properties a request ID must never have.
func newRequestID() string {
	buf := make([]byte, 8)
	if _, err := crand.Read(buf); err != nil {
		// The OS entropy source itself failing is vanishingly rare and
		// not worth failing the request over; fall back to a
		// timestamp-based ID so the request still gets one, just a less
		// unique one.
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(buf)
}
