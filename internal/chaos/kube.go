package chaos

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// Outcome label values for buddy_chaos_actions_total's "outcome" label.
// Kept here, next to Execute (their only producer), so cmd/chaos-buddy
// never has to invent its own strings for the metric it emits.
const (
	// OutcomeSuccess is a real action that completed without error.
	OutcomeSuccess = "success"
	// OutcomeFailure is a real action that was attempted and failed.
	OutcomeFailure = "failure"
	// OutcomeDryRun is an action that was decided but not performed,
	// because dry-run is enabled.
	OutcomeDryRun = "dry-run"
)

// switchKey is the single ConfigMap data key ReadSwitch reads.
const switchKey = "enabled"

// chaosReadinessPath and podHTTPPort locate a target pod's chaos endpoint.
// buddy-api always listens on 8080 (see cmd/buddy-api/main.go's BUDDY_PORT
// default, which the shipped ConfigMap never overrides -- see
// internal/controller/resources.go's ConfigMapFor comment for why not), so
// this is a fixed constant, not something chaos-buddy discovers per pod.
const (
	chaosReadinessPath = "/chaos/readiness"
	podHTTPPort        = 8080
)

// readinessFlapWindow bounds how long ModeReadinessFlap holds a target pod
// unready before restoring it. It is a fixed constant, not an
// operator-tunable setting: the safety story for this mode is that a run
// can only ever hold a target unready for a small, fixed, code-reviewed
// window -- fifteen seconds is enough to be visible in a scrape interval
// and in a demo, and short enough that a single flap can never be mistaken
// for (or become) a genuine outage.
const readinessFlapWindow = 15 * time.Second

// httpTimeout bounds a single request to a target pod's /chaos/readiness
// endpoint. The pod is reached directly by IP, in-cluster, so this only
// needs to guard against a genuinely wedged target, not network latency.
const httpTimeout = 5 * time.Second

// PodClient is everything Execute (and cmd/chaos-buddy's loop) needs from
// the Kubernetes API and from a target pod's own HTTP surface. It exists
// so engine_test.go can substitute a fake that records which destructive
// methods were called, without importing client-go: every method here is
// spelled entirely in stdlib and this package's own PodRef/time types, so
// the interface itself carries no client-go dependency even though Client
// (below) implements it using client-go.
type PodClient interface {
	// ListPods returns every pod in namespace matching labelSelector.
	ListPods(ctx context.Context, namespace, labelSelector string) ([]PodRef, error)
	// DeletePod deletes name in namespace. Deleting a pod that is already
	// gone is not an error -- see Client.DeletePod's comment.
	DeletePod(ctx context.Context, namespace, name string) error
	// FlapReadiness sets pod unready, holds it unready for window, then
	// restores it.
	FlapReadiness(ctx context.Context, pod PodRef, window time.Duration) error
	// EmitEvent creates a Kubernetes Event whose InvolvedObject is pod.
	EmitEvent(ctx context.Context, pod PodRef, reason, message, eventType string) error
	// ReadSwitch reads the "enabled" key of the ConfigMap name in
	// namespace, returning an error (never a partial/default value) if the
	// ConfigMap, the key, or the value's boolean parse fails.
	ReadSwitch(ctx context.Context, namespace, name string) (bool, error)
}

// Client is PodClient's real implementation, backed by a client-go
// clientset for every Kubernetes API call and a plain *http.Client for the
// direct-to-pod /chaos/readiness call ModeReadinessFlap needs.
type Client struct {
	clientset kubernetes.Interface
	http      *http.Client
}

// NewClient builds a Client. httpClient may be nil, in which case a client
// with httpTimeout is constructed; tests and cmd/chaos-buddy both normally
// pass their own so the timeout is explicit at the call site.
func NewClient(clientset kubernetes.Interface, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: httpTimeout}
	}
	return &Client{clientset: clientset, http: httpClient}
}

var _ PodClient = (*Client)(nil)

// ListPods lists namespace's pods matching labelSelector and translates
// each into a PodRef. Errors are wrapped with the namespace and selector
// that were being queried, since a bare client-go error ("forbidden", "no
// such host") gives a reader nothing to act on without that context.
func (c *Client) ListPods(ctx context.Context, namespace, labelSelector string) ([]PodRef, error) {
	list, err := c.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return nil, fmt.Errorf("list pods in namespace %q matching selector %q: %w", namespace, labelSelector, err)
	}

	pods := make([]PodRef, 0, len(list.Items))
	for _, p := range list.Items {
		pods = append(pods, PodRef{
			Name:      p.Name,
			Namespace: p.Namespace,
			UID:       string(p.UID),
			IP:        p.Status.PodIP,
		})
	}
	return pods, nil
}

// DeletePod deletes name in namespace. A NotFound error is swallowed
// rather than surfaced: the target is already gone by the time this call
// lands (a concurrent delete, a rollout, a prior chaos-buddy iteration
// that raced with a slow reconcile), which is the same end state a
// successful delete would have produced -- treating it as a failure would
// make buddy_chaos_actions_total{outcome="failure"} climb for an outcome
// that was, from chaos-buddy's perspective, entirely successful.
func (c *Client) DeletePod(ctx context.Context, namespace, name string) error {
	err := c.clientset.CoreV1().Pods(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete pod %s/%s: %w", namespace, name, err)
	}
	return nil
}

// readinessBody is the JSON body POSTed to /chaos/readiness, mirroring
// internal/api's chaosReadinessRequest exactly (field name and JSON tag).
type readinessBody struct {
	Ready bool `json:"ready"`
}

// FlapReadiness sets pod unready, waits window (or until ctx is done,
// whichever comes first), then restores it. Both requests go directly to
// pod.IP rather than through a Service, so the flap always lands on the
// pod Decide actually selected instead of whichever replica a Service
// happens to load-balance to.
//
// A failure on the FIRST call (setting unready) aborts before the wait --
// there is nothing to restore if the pod was never made unready. A failure
// on the restoring call is still returned, but only after the window has
// already elapsed: callers must not assume a non-nil error here means the
// target was never made unready, since by construction it was.
func (c *Client) FlapReadiness(ctx context.Context, pod PodRef, window time.Duration) error {
	if pod.IP == "" {
		return fmt.Errorf("readiness-flap: pod %s/%s has no assigned IP yet", pod.Namespace, pod.Name)
	}

	url := fmt.Sprintf("http://%s:%d%s", pod.IP, podHTTPPort, chaosReadinessPath)

	if err := postReadiness(ctx, c.http, url, false); err != nil {
		return fmt.Errorf(
			"readiness-flap: set pod %s/%s unready: %w (this endpoint only exists when the target's own "+
				"BUDDY_ENABLE_CHAOS_ENDPOINTS=true; that's driven by the target Plant's spec.chaos.enableEndpoints "+
				"-- see api/v1alpha1/plant_types.go's ChaosSpec and internal/controller/resources.go's ConfigMapFor "+
				"-- and defaults to false, so readiness-flap will fail loudly exactly like this against a Plant "+
				"that has not opted in)",
			pod.Namespace, pod.Name, err,
		)
	}

	select {
	case <-time.After(window):
	case <-ctx.Done():
		return fmt.Errorf("readiness-flap: context canceled while pod %s/%s was held unready: %w", pod.Namespace, pod.Name, ctx.Err())
	}

	if err := postReadiness(ctx, c.http, url, true); err != nil {
		return fmt.Errorf("readiness-flap: restore pod %s/%s to ready: %w", pod.Namespace, pod.Name, err)
	}
	return nil
}

// postReadiness sends one POST /chaos/readiness request with the given
// ready value and treats any non-200 response, or any transport error, as
// a failure.
func postReadiness(ctx context.Context, hc *http.Client, url string, ready bool) error {
	payload, err := json.Marshal(readinessBody{Ready: ready})
	if err != nil {
		return fmt.Errorf("encode readiness body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("POST %s: unexpected status %d (endpoint may not be registered -- see BUDDY_ENABLE_CHAOS_ENDPOINTS)", url, resp.StatusCode)
	}
	return nil
}

// EmitEvent creates a Kubernetes Event whose InvolvedObject is pod, with
// reason, message, and eventType (corev1.EventTypeNormal or
// corev1.EventTypeWarning) exactly as given. GenerateName (rather than a
// fixed Name) is used so multiple chaos-buddy actions against the same pod
// never collide on Event identity -- Kubernetes generates a unique suffix
// per Create.
func (c *Client) EmitEvent(ctx context.Context, pod PodRef, reason, message, eventType string) error {
	now := metav1.Now()
	event := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "chaos-buddy-",
			Namespace:    pod.Namespace,
		},
		InvolvedObject: corev1.ObjectReference{
			APIVersion: "v1",
			Kind:       "Pod",
			Namespace:  pod.Namespace,
			Name:       pod.Name,
			UID:        types.UID(pod.UID),
		},
		Reason:         reason,
		Message:        message,
		Type:           eventType,
		Source:         corev1.EventSource{Component: "chaos-buddy"},
		FirstTimestamp: now,
		LastTimestamp:  now,
		Count:          1,
	}

	if _, err := c.clientset.CoreV1().Events(pod.Namespace).Create(ctx, event, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("emit event on pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	return nil
}

// ReadSwitch reads the kill-switch ConfigMap's "enabled" key. Every failure
// mode -- the ConfigMap doesn't exist, the key is missing, the value isn't
// a valid boolean -- returns an error and enabled=false, never a
// best-effort guess: SwitchPermits (engine.go) treats any non-nil error
// here as fail-closed regardless of the bool it's paired with, but
// returning false alongside the error keeps this function's own contract
// honest for any other caller.
func (c *Client) ReadSwitch(ctx context.Context, namespace, name string) (bool, error) {
	cm, err := c.clientset.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("read kill switch configmap %s/%s: %w", namespace, name, err)
	}

	raw, ok := cm.Data[switchKey]
	if !ok {
		return false, fmt.Errorf("kill switch configmap %s/%s has no %q key", namespace, name, switchKey)
	}

	enabled, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("kill switch configmap %s/%s key %q = %q is not a valid boolean: %w", namespace, name, switchKey, raw, err)
	}
	return enabled, nil
}

// actionNarration returns the verb (for log/event messages) and the
// CamelCase Event reason for kind. Used only by Execute, kept as a small
// pure lookup rather than duplicated across Execute's branches.
func actionNarration(kind ActionKind) (verb, reason string) {
	switch kind {
	case ActionKillPod:
		return "kill", "ChaosPodKill"
	case ActionFlapReadiness:
		return "flap readiness on", "ChaosReadinessFlap"
	default:
		return "act on", "ChaosUnknown"
	}
}

// Execute carries out (or, in dry-run, only narrates) the action decision
// describes, using client for every Kubernetes and pod-HTTP call. It
// returns one of OutcomeSuccess, OutcomeFailure, or OutcomeDryRun -- the
// exact value cmd/chaos-buddy records as buddy_chaos_actions_total's
// "outcome" label -- and, on failure, the error that caused it.
//
// decision.Kind == ActionNone is a caller error (Decide already reported
// nothing to do) and Execute treats it as a no-op returning ("", nil)
// rather than touching client at all.
//
// Every other path emits a Kubernetes Event on decision.Target before
// returning, including the dry-run path -- so `kubectl describe pod` tells
// the same story whether or not chaos-buddy was actually allowed to act,
// which is the point of shipping dry-run: true by default rather than
// simply not deploying chaos-buddy at all.
//
// dry-run's safety guarantee lives entirely in the branching below: the
// destructive switch over DeletePod/FlapReadiness is only reachable once
// the `if dryRun` block has already returned. engine_test.go's fake
// PodClient asserts this directly, by leaving DeletePod/FlapReadiness
// wired to fail the test if called and then running Execute with
// dryRun=true.
func Execute(ctx context.Context, client PodClient, decision Decision, dryRun bool) (outcome string, err error) {
	if decision.Kind == ActionNone {
		return "", nil
	}

	verb, reason := actionNarration(decision.Kind)

	if dryRun {
		msg := fmt.Sprintf("[dry-run] chaos-buddy would %s pod %s/%s", verb, decision.Target.Namespace, decision.Target.Name)
		if evErr := client.EmitEvent(ctx, decision.Target, reason, msg, corev1.EventTypeNormal); evErr != nil {
			return OutcomeDryRun, fmt.Errorf("dry-run: emit intent event: %w", evErr)
		}
		return OutcomeDryRun, nil
	}

	var actionErr error
	switch decision.Kind {
	case ActionKillPod:
		actionErr = client.DeletePod(ctx, decision.Target.Namespace, decision.Target.Name)
	case ActionFlapReadiness:
		actionErr = client.FlapReadiness(ctx, decision.Target, readinessFlapWindow)
	default:
		actionErr = fmt.Errorf("execute: unrecognized action kind %q", decision.Kind)
	}

	if actionErr != nil {
		msg := fmt.Sprintf("chaos-buddy attempted to %s pod %s/%s and failed: %v", verb, decision.Target.Namespace, decision.Target.Name, actionErr)
		// Best-effort: the action already failed, and a failure to also
		// emit the event describing that failure must not mask the
		// original error or stop Execute from returning it.
		_ = client.EmitEvent(ctx, decision.Target, reason, msg, corev1.EventTypeWarning)
		return OutcomeFailure, actionErr
	}

	msg := fmt.Sprintf("chaos-buddy %s pod %s/%s", verb, decision.Target.Namespace, decision.Target.Name)
	if evErr := client.EmitEvent(ctx, decision.Target, reason, msg, corev1.EventTypeWarning); evErr != nil {
		return OutcomeSuccess, fmt.Errorf("action succeeded but emitting its event failed: %w", evErr)
	}
	return OutcomeSuccess, nil
}
