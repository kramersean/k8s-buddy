package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/sean-kramer/k8s-buddy/internal/telemetry"
)

// unmatchedRoutePattern is the stable placeholder the metrics middleware
// (see withMetrics in middleware.go) records for any request that
// matched no registered route, instead of ever recording the raw
// request path.
const unmatchedRoutePattern = "unmatched"

// statusBody is the JSON body for the simple probe endpoints (/healthz,
// /readyz) and for lightweight error responses.
type statusBody struct {
	// Status is a short human-readable status word.
	Status string `json:"status"`
}

// healthzHandler implements the liveness probe. It ALWAYS returns 200
// while this process is alive and able to run a handler at all -- it
// deliberately never consults s.ready, the mood engine, or the health
// score.
//
// Liveness and readiness answer two different questions, and conflating
// them is one of the most common Kubernetes misconfigurations: liveness
// asks "is this process wedged and does it need to be killed and
// restarted?"; readiness asks "should traffic be sent here right now?" A
// plant that's merely thirsty, or a pod chaos-buddy has deliberately
// marked not-ready for a demo, is neither wedged nor broken -- it's
// perfectly healthy code accurately reporting bad news about a business
// condition. If healthz mirrored readyz, the kubelet would restart the
// pod every time readiness dipped, turning a normal, recoverable "not
// ready" window into a crash-loop: it would destroy the very in-flight
// requests graceful shutdown exists to protect, reset any in-memory
// state, and hide the real signal (readiness) behind a stream of
// container restarts. So healthz only ever fails if this handler can't
// run at all -- which, by definition, means nothing here could have
// answered "not ok" anyway.
func (s *Server) healthzHandler(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, statusBody{Status: "ok"})
}

// readyzHandler implements the readiness probe: 200 while ready, 503
// with {"status":"not ready"} otherwise, reflecting whatever SetReady
// last recorded. This is the endpoint /chaos/readiness flips and the one
// the kubelet uses to add or remove this pod from Service endpoints.
// Unlike healthz, a 503 here never triggers a restart -- only a
// temporary removal from load-balancing, which is exactly the effect a
// "don't route traffic here right now" signal should have.
func (s *Server) readyzHandler(w http.ResponseWriter, _ *http.Request) {
	if !s.ready.Load() {
		s.writeJSON(w, http.StatusServiceUnavailable, statusBody{Status: "not ready"})
		return
	}
	s.writeJSON(w, http.StatusOK, statusBody{Status: "ready"})
}

// statusHandler implements /status. The response body is a mood.Report
// built exclusively by mood.NewReport -- never assembled field by field
// -- so Mood, Message, and HealthScore can never drift out of sync with
// one another the way they could if this handler derived a score and a
// mood independently and one of the two got updated without the other.
// It also pushes the freshly built report into Prometheus via
// syncMetrics, so a scrape landing right after this call sees the same
// numbers this response just returned.
func (s *Server) statusHandler(w http.ResponseWriter, _ *http.Request) {
	report := s.currentReport()
	s.syncMetrics(report)
	s.writeJSON(w, http.StatusOK, report)
}

// workResponse is the /work response body.
type workResponse struct {
	// Outcome is one of telemetry.OutcomeSuccess,
	// telemetry.OutcomeWarning, or telemetry.OutcomeFailure -- the same
	// vocabulary the "outcome" metric label uses, from the same constants,
	// so the JSON body and the metrics can never disagree.
	Outcome string `json:"outcome"`
	// DelayMs is the simulated delay this request slept for, in
	// milliseconds.
	DelayMs int64 `json:"delayMs"`
	// Message is a human-readable description of Outcome.
	Message string `json:"message"`
}

// workHandler simulates a unit of plant-care work. It sleeps a random
// duration in [WorkMinDelay, WorkMaxDelay], then classifies the request
// as a failure (WorkErrorRate probability, HTTP 500), a warning (HTTP
// 200, when the sampled delay exceeds LatencyBudget), or a success (HTTP
// 200) -- in that priority order, so a slow-and-also-unlucky request
// reports as a failure, not a warning. Every outcome is recorded via
// ObserveWork before the response is written, so
// buddy_work_requests_total and buddy_work_duration_seconds always agree
// with what the caller actually received.
//
// The same observation is also pushed into s.work, the rolling window
// /status derives its ErrorRate and P95Latency signals from -- so calling
// /work is what actually moves the plant's mood. Both recordings happen
// before the response is written, so a client that reads /status
// immediately after its /work response comes back is guaranteed to see
// that request already reflected.
func (s *Server) workHandler(w http.ResponseWriter, _ *http.Request) {
	delay := s.randomWorkDelay()
	time.Sleep(delay)

	outcome, status := s.sampleWorkOutcome(delay)

	s.work.observe(delay, outcome == telemetry.OutcomeFailure)
	s.metrics.ObserveWork(outcome, delay)

	resp := workResponse{
		Outcome: outcome,
		DelayMs: delay.Milliseconds(),
		Message: workMessage(outcome, delay, s.cfg.LatencyBudget),
	}
	s.writeJSON(w, status, resp)
}

// workMessage returns the human-readable message for a /work outcome.
// The warning case gets a body distinct from success -- naming the
// budget it exceeded -- so a dashboard or a curious human can tell "slow
// but technically fine" apart from "everything's great" without
// cross-referencing the HTTP status code, which is identical (200) for
// both.
func workMessage(outcome string, delay, budget time.Duration) string {
	switch outcome {
	case telemetry.OutcomeFailure:
		return "dropped a leaf: simulated work failure"
	case telemetry.OutcomeWarning:
		return fmt.Sprintf("took %s, over the %s latency budget", delay, budget)
	default:
		return "watered on time"
	}
}

// randomWorkDelay samples a duration uniformly from the CLOSED interval
// [WorkMinDelay, WorkMaxDelay] -- both endpoints included -- using the
// server's injected (or time-seeded) *rand.Rand. Int63n(n) itself only
// ever returns a half-open [0,n), so span+1 is passed rather than span,
// making WorkMaxDelay itself a reachable outcome and not an off-by-one
// exclusive bound. cmd/buddy-api validates MinDelay <= MaxDelay at
// startup; the equal-or-inverted-bounds fallback here only guards
// callers -- tests, mainly -- that build a Config directly without going
// through that validation.
func (s *Server) randomWorkDelay() time.Duration {
	lo, hi := s.cfg.WorkMinDelay, s.cfg.WorkMaxDelay
	if hi <= lo {
		return lo
	}

	span := int64(hi - lo)

	s.randMu.Lock()
	defer s.randMu.Unlock()
	//nolint:gosec // G404: simulated /work latency, not security-sensitive;
	// the source is intentionally injectable (Config.Rand) for test determinism.
	return lo + time.Duration(s.rand.Int63n(span+1))
}

// sampleWorkOutcome classifies a /work request given its already-sampled
// delay: a failure with probability WorkErrorRate, else a warning when
// delay exceeds LatencyBudget, else a success. A LatencyBudget <= 0 is
// treated as "no budget configured" and never produces a warning,
// mirroring how mood.Signals.Score treats a non-positive LatencyBudget as
// awarding full latency marks.
func (s *Server) sampleWorkOutcome(delay time.Duration) (outcome string, status int) {
	s.randMu.Lock()
	//nolint:gosec // G404: simulated /work failure sampling, not security-sensitive.
	roll := s.rand.Float64()
	s.randMu.Unlock()

	if roll < s.cfg.WorkErrorRate {
		return telemetry.OutcomeFailure, http.StatusInternalServerError
	}
	if s.cfg.LatencyBudget > 0 && delay > s.cfg.LatencyBudget {
		return telemetry.OutcomeWarning, http.StatusOK
	}
	return telemetry.OutcomeSuccess, http.StatusOK
}

// chaosReadinessRequest is the POST /chaos/readiness request body.
type chaosReadinessRequest struct {
	// Ready is the readiness value to set.
	Ready bool `json:"ready"`
}

// chaosReadinessHandler flips readiness on command. Handler only ever
// registers this on the mux when Config.EnableChaosEndpoints is true, so
// there is no runtime "is this feature enabled" check inside the handler
// itself to bypass -- when the feature is off, this method is simply
// never reachable, because the route was never added.
func (s *Server) chaosReadinessHandler(w http.ResponseWriter, r *http.Request) {
	var body chaosReadinessRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, statusBody{Status: "invalid request body"})
		return
	}

	s.SetReady(body.Ready)
	s.log.Warn("chaos: readiness overridden via /chaos/readiness", "ready", body.Ready)
	s.writeJSON(w, http.StatusOK, statusBody{Status: fmt.Sprintf("readiness set to %t", body.Ready)})
}

// writeJSON writes body as a JSON-encoded response with the given status
// code. An encoding failure at this point can't change what's already
// been sent -- the status line and headers are written first -- so the
// most useful thing to do with that error is log it rather than discard
// it silently.
func (s *Server) writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		s.log.Error("failed to encode JSON response", "error", err)
	}
}
