// Package chaos implements chaos-buddy's decision logic and Kubernetes
// client calls, deliberately split across two files:
//
//   - engine.go (this file) holds PURE decision logic: which pod to pick
//     from a candidate list, whether the kill switch permits an action,
//     whether a target is inside the allowed namespace, and what the next
//     action should be given a mode and the current state. Nothing in this
//     file touches a network, a clock (beyond an injected *rand.Rand), or
//     the Kubernetes API -- every function here is a pure function of its
//     arguments, so engine_test.go exercises all of it without a cluster,
//     without envtest, and without even a fake HTTP server.
//   - kube.go holds every actual client call: listing and deleting pods,
//     flipping a target's readiness over HTTP, creating the Kubernetes
//     Event that makes each action visible on `kubectl describe pod`, and
//     reading the kill-switch ConfigMap. It is the only file in this
//     package that imports client-go.
//
// cmd/chaos-buddy/main.go is the only caller of both: it loads and
// validates configuration from the environment, builds a real client
// (kube.go's Client), and drives the loop that calls Decide and Execute
// once per CHAOS_INTERVAL.
package chaos

import (
	"errors"
	"fmt"
	"math/rand"
	"strings"
)

// Mode is chaos-buddy's failure-injection mode. There are exactly two --
// see ParseMode's comment for why "exactly two" is a deliberate design
// decision, not a placeholder for more to come later.
type Mode string

const (
	// ModePodKill deletes one pod matching CHAOS_LABEL_SELECTOR in
	// CHAOS_TARGET_NAMESPACE.
	ModePodKill Mode = "pod-kill"
	// ModeReadinessFlap POSTs {"ready":false} to a target pod's
	// /chaos/readiness endpoint, holds it unready for a bounded window,
	// then restores it. It requires the target's BUDDY_ENABLE_CHAOS_ENDPOINTS
	// to be true -- see FlapReadiness in kube.go for what happens when it
	// isn't.
	ModeReadinessFlap Mode = "readiness-flap"
)

// SupportedModes returns every mode chaos-buddy implements, in the order
// ParseMode's error message lists them. This is the single source of truth
// for "which modes exist" -- ParseMode and every message that names the
// supported set derive from it, so adding or removing a mode never
// requires updating a second list by hand.
func SupportedModes() []Mode {
	return []Mode{ModePodKill, ModeReadinessFlap}
}

// ParseMode validates s against SupportedModes and returns the
// corresponding Mode, or an error naming both the bad value and the exact
// supported set.
//
// This project's chaos plan originally named five modes: pod-kill,
// readiness-flap, latency, cpu-burn, and oom. Only the first two are
// implemented against real behavior in buddy-api; the other three would
// have been flags that appeared to work and silently did nothing --
// "shipping a knob that doesn't turn anything" is worse than the knob not
// existing, because a reviewer (or an on-call engineer, in a real system)
// has no way to distinguish "did nothing because chaos is disabled" from
// "did nothing because this mode was never real." ParseMode is the
// enforcement point: an unsupported mode is a startup error, not a no-op.
func ParseMode(s string) (Mode, error) {
	for _, m := range SupportedModes() {
		if Mode(s) == m {
			return m, nil
		}
	}
	return "", fmt.Errorf(
		"invalid CHAOS_MODE %q: chaos-buddy implements only %q and %q (latency, cpu-burn, and oom were dropped rather than stubbed)",
		s, ModePodKill, ModeReadinessFlap,
	)
}

// ValidateLabelSelector rejects an empty (or whitespace-only) selector.
// chaos-buddy has no concept of "match everything" -- an operator who
// wants broad blast radius must say so explicitly with a selector that
// matches broadly (e.g. a shared component label), never by omission. An
// empty CHAOS_LABEL_SELECTOR is therefore always a startup error, the same
// validation-not-silent-fallback posture cmd/buddy-api's loadConfig uses
// for every other setting that has no safe default.
func ValidateLabelSelector(selector string) error {
	if strings.TrimSpace(selector) == "" {
		return errors.New(
			"CHAOS_LABEL_SELECTOR must not be empty: chaos-buddy refuses to run with an implicit \"match everything\" selector",
		)
	}
	return nil
}

// PodRef is a minimal, engine-owned description of a pod: just enough for
// selection, namespace refusal, and (once an action is decided) for kube.go
// to act on. It is deliberately not corev1.Pod -- carrying a client-go type
// into this file would drag the whole client-go dependency graph into a
// package whose entire point is being testable without one. kube.go's
// ListPods is responsible for translating live Pods into these.
type PodRef struct {
	// Name is the pod's name.
	Name string
	// Namespace is the pod's namespace.
	Namespace string
	// UID is the pod's Kubernetes UID, as a string. Used only to populate
	// the InvolvedObject of the Kubernetes Event kube.go emits, so a
	// concurrent pod recreation with the same name can't cause an event to
	// attach to the wrong object.
	UID string
	// IP is the pod's assigned Pod IP, or "" if it has none yet (e.g. still
	// scheduling). Only ModeReadinessFlap uses it, to call the target's
	// /chaos/readiness endpoint directly rather than through a Service,
	// which would load-balance the request to an arbitrary replica instead
	// of the one that was actually selected.
	IP string
}

// SelectPod deterministically picks one pod from candidates using r: given
// the same candidates in the same order and an r seeded identically,
// SelectPod always returns the same pod. An empty candidate list is a
// no-op, not a panic -- it returns the zero PodRef and ok=false, which
// every caller (Decide, in this file) treats as "nothing to do this
// iteration" rather than an error, since "no pod currently matches the
// selector" is an entirely ordinary state (a Deployment mid-rollout, or a
// selector that's briefly over-narrow) and not a configuration mistake.
func SelectPod(candidates []PodRef, r *rand.Rand) (PodRef, bool) {
	if len(candidates) == 0 {
		return PodRef{}, false
	}
	idx := r.Intn(len(candidates))
	return candidates[idx], true
}

// SwitchPermits reports whether the kill switch currently permits a chaos
// action, given the ConfigMap's last-read "enabled" value and any error
// encountered reading it.
//
// A read failure ALWAYS fails closed (returns false), regardless of what
// enabled holds -- enabled is meaningless when readErr != nil (kube.go
// always returns the zero value alongside a non-nil error) but this
// function is deliberately defensive about that rather than trusting every
// caller to zero it correctly. The alternative -- treating an unreadable
// switch as "permitted" because the last known value happened to be true,
// or because a transient API error shouldn't block chaos -- is exactly
// backwards for a tool whose entire purpose is deleting pods: the failure
// mode of "can't confirm chaos is allowed" must be "don't," never "sure,
// probably." This is also what cmd/chaos-buddy's buddy_chaos_enabled gauge
// reports directly: SwitchPermits's return value IS that gauge's value.
func SwitchPermits(enabled bool, readErr error) bool {
	if readErr != nil {
		return false
	}
	return enabled
}

// InTargetNamespace reports whether pod belongs to targetNamespace. This is
// the pure half of chaos-buddy's namespace refusal -- the belt to the
// RBAC Role's braces (deploy/kustomize/chaos/rbac.yaml grants delete only
// within one namespace, so the API server itself would reject a
// cross-namespace delete): even if a future change to ListPods's caller
// ever passed it the wrong namespace, or a candidate slice were assembled
// by hand (as engine_test.go's refusal case does), Decide below still
// refuses to act on anything outside CHAOS_TARGET_NAMESPACE. An empty
// namespace never matches, even against an empty targetNamespace, since a
// PodRef with no namespace is not a validly-identified pod.
func InTargetNamespace(pod PodRef, targetNamespace string) bool {
	return pod.Namespace != "" && pod.Namespace == targetNamespace
}

// ActionKind identifies what Decide has decided to do.
type ActionKind string

const (
	// ActionNone means: do nothing this iteration. Reason on the returned
	// Decision explains why.
	ActionNone ActionKind = "none"
	// ActionKillPod means: delete Target (ModePodKill).
	ActionKillPod ActionKind = "kill-pod"
	// ActionFlapReadiness means: flip Target's readiness off, then back on,
	// via its /chaos/readiness endpoint (ModeReadinessFlap).
	ActionFlapReadiness ActionKind = "flap-readiness"
)

// Decision is Decide's result: either a concrete action against a specific
// target, or ActionNone with a human-readable Reason explaining the
// refusal or the absence of anything to do.
type Decision struct {
	// Kind is the action to take, or ActionNone.
	Kind ActionKind
	// Target is the pod Kind applies to. Zero-valued when Kind is
	// ActionNone.
	Target PodRef
	// Reason explains an ActionNone decision. Empty for any other Kind.
	Reason string
}

// Decide computes chaos-buddy's next action for one loop iteration. It
// composes every other pure function in this file, in the order that
// matters for safety:
//
//  1. SwitchPermits -- if the kill switch doesn't permit action (disabled,
//     or unreadable and therefore failed closed), refuse before even
//     looking at candidates.
//  2. SelectPod -- pick a target from candidates. An empty list refuses.
//  3. InTargetNamespace -- refuse if the selected pod is somehow outside
//     targetNamespace, even though candidates should already be scoped to
//     it by the caller's List call.
//  4. mode -- translate an approved, in-namespace target into the action
//     that mode implies.
//
// Any refusal short-circuits: Decide never reaches a later check once an
// earlier one has already said no, and the returned Decision always
// explains which check stopped it.
func Decide(mode Mode, switchEnabled bool, switchReadErr error, targetNamespace string, candidates []PodRef, r *rand.Rand) Decision {
	if !SwitchPermits(switchEnabled, switchReadErr) {
		reason := "kill switch is disabled"
		if switchReadErr != nil {
			reason = fmt.Sprintf("kill switch ConfigMap could not be read, failing closed: %v", switchReadErr)
		}
		return Decision{Kind: ActionNone, Reason: reason}
	}

	target, ok := SelectPod(candidates, r)
	if !ok {
		return Decision{Kind: ActionNone, Reason: "no candidate pods matched CHAOS_LABEL_SELECTOR"}
	}

	if !InTargetNamespace(target, targetNamespace) {
		return Decision{
			Kind: ActionNone,
			Reason: fmt.Sprintf(
				"selected pod %s/%s is outside CHAOS_TARGET_NAMESPACE %q; refusing to act",
				target.Namespace, target.Name, targetNamespace,
			),
		}
	}

	switch mode {
	case ModePodKill:
		return Decision{Kind: ActionKillPod, Target: target}
	case ModeReadinessFlap:
		return Decision{Kind: ActionFlapReadiness, Target: target}
	default:
		// Unreachable in the normal flow, since cmd/chaos-buddy validates
		// CHAOS_MODE through ParseMode before Decide is ever called -- but
		// this stays an explicit refusal rather than a fallthrough that
		// would otherwise return a zero-valued Decision{} (ActionKind ""),
		// which is not the same thing as ActionNone and would confuse any
		// caller switching on Kind.
		return Decision{Kind: ActionNone, Reason: fmt.Sprintf("unsupported mode %q", mode)}
	}
}
