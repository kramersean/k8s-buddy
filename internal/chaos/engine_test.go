package chaos

import (
	"context"
	"errors"
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakePodClient is a PodClient test double that records every call it
// receives instead of touching a cluster. It implements PodClient using
// only stdlib and this package's own types, so this file never needs to
// import client-go -- proving, by construction, that Execute's caller
// contract really is testable without a cluster.
type fakePodClient struct {
	listPods []PodRef
	listErr  error

	deleteCalled bool
	deleteErr    error

	flapCalled bool
	flapErr    error

	switchEnabled bool
	switchErr     error

	events []recordedEvent

	// failOnDestructiveCall, when set, makes DeletePod/FlapReadiness call
	// t.Fatal instead of just recording -- used by the dry-run test so a
	// regression that starts calling the destructive path fails loudly at
	// the exact point it happens, not just via a bool assertion after the
	// fact.
	t *testing.T
}

type recordedEvent struct {
	pod       PodRef
	reason    string
	message   string
	eventType string
}

func (f *fakePodClient) ListPods(_ context.Context, _, _ string) ([]PodRef, error) {
	return f.listPods, f.listErr
}

func (f *fakePodClient) DeletePod(_ context.Context, _, _ string) error {
	if f.t != nil {
		f.t.Fatal("DeletePod must not be called")
	}
	f.deleteCalled = true
	return f.deleteErr
}

func (f *fakePodClient) FlapReadiness(_ context.Context, _ PodRef, _ time.Duration) error {
	if f.t != nil {
		f.t.Fatal("FlapReadiness must not be called")
	}
	f.flapCalled = true
	return f.flapErr
}

func (f *fakePodClient) EmitEvent(_ context.Context, pod PodRef, reason, message, eventType string) error {
	f.events = append(f.events, recordedEvent{pod: pod, reason: reason, message: message, eventType: eventType})
	return nil
}

func (f *fakePodClient) ReadSwitch(_ context.Context, _, _ string) (bool, error) {
	return f.switchEnabled, f.switchErr
}

// --- SelectPod ---------------------------------------------------------

func TestSelectPod_DeterministicWithSeededRand(t *testing.T) {
	candidates := []PodRef{
		{Name: "a", Namespace: "ns"},
		{Name: "b", Namespace: "ns"},
		{Name: "c", Namespace: "ns"},
		{Name: "d", Namespace: "ns"},
	}

	first, ok := SelectPod(candidates, rand.New(rand.NewSource(42)))
	require.True(t, ok)

	second, ok := SelectPod(candidates, rand.New(rand.NewSource(42)))
	require.True(t, ok)

	assert.Equal(t, first, second, "the same seed against the same candidates must select the same pod")
}

func TestSelectPod_EmptyList_NoPanic(t *testing.T) {
	assert.NotPanics(t, func() {
		pod, ok := SelectPod(nil, rand.New(rand.NewSource(1)))
		assert.False(t, ok)
		assert.Equal(t, PodRef{}, pod)
	})

	assert.NotPanics(t, func() {
		pod, ok := SelectPod([]PodRef{}, rand.New(rand.NewSource(1)))
		assert.False(t, ok)
		assert.Equal(t, PodRef{}, pod)
	})
}

// --- SwitchPermits -------------------------------------------------------

func TestSwitchPermits(t *testing.T) {
	tests := []struct {
		name    string
		enabled bool
		readErr error
		want    bool
	}{
		{name: "enabled and readable", enabled: true, readErr: nil, want: true},
		{name: "disabled and readable", enabled: false, readErr: nil, want: false},
		{name: "enabled but unreadable fails closed", enabled: true, readErr: errors.New("configmap get failed"), want: false},
		{name: "disabled and unreadable stays closed", enabled: false, readErr: errors.New("configmap get failed"), want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, SwitchPermits(tc.enabled, tc.readErr))
		})
	}
}

// --- InTargetNamespace ---------------------------------------------------

func TestInTargetNamespace(t *testing.T) {
	tests := []struct {
		name   string
		pod    PodRef
		target string
		want   bool
	}{
		{name: "matching namespace", pod: PodRef{Name: "a", Namespace: "k8s-buddy-plants"}, target: "k8s-buddy-plants", want: true},
		{name: "different namespace", pod: PodRef{Name: "a", Namespace: "kube-system"}, target: "k8s-buddy-plants", want: false},
		{name: "empty pod namespace never matches", pod: PodRef{Name: "a", Namespace: ""}, target: "", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, InTargetNamespace(tc.pod, tc.target))
		})
	}
}

// --- ParseMode -----------------------------------------------------------

func TestParseMode(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    Mode
		wantErr bool
	}{
		{name: "pod-kill accepted", input: "pod-kill", want: ModePodKill},
		{name: "readiness-flap accepted", input: "readiness-flap", want: ModeReadinessFlap},
		{name: "latency rejected", input: "latency", wantErr: true},
		{name: "cpu-burn rejected", input: "cpu-burn", wantErr: true},
		{name: "oom rejected", input: "oom", wantErr: true},
		{name: "empty rejected", input: "", wantErr: true},
		{name: "garbage rejected", input: "not-a-mode", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseMode(tc.input)
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "pod-kill")
				assert.Contains(t, err.Error(), "readiness-flap")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// --- ValidateLabelSelector -------------------------------------------------

func TestValidateLabelSelector(t *testing.T) {
	tests := []struct {
		name     string
		selector string
		wantErr  bool
	}{
		{name: "empty rejected", selector: "", wantErr: true},
		{name: "whitespace-only rejected", selector: "   ", wantErr: true},
		{name: "simple selector accepted", selector: "buddy.k8s-buddy.io/plant=fernie", wantErr: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateLabelSelector(tc.selector)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

// --- Decide ----------------------------------------------------------------

func TestDecide(t *testing.T) {
	inNS := PodRef{Name: "fernie-1", Namespace: "k8s-buddy-plants"}
	outOfNS := PodRef{Name: "sneaky", Namespace: "kube-system"}

	tests := []struct {
		name            string
		mode            Mode
		switchEnabled   bool
		switchReadErr   error
		targetNamespace string
		candidates      []PodRef
		wantKind        ActionKind
		wantTarget      PodRef
	}{
		{
			name:            "kill switch disabled blocks action",
			mode:            ModePodKill,
			switchEnabled:   false,
			targetNamespace: "k8s-buddy-plants",
			candidates:      []PodRef{inNS},
			wantKind:        ActionNone,
		},
		{
			name:            "configmap read failure fails closed even though enabled=true",
			mode:            ModePodKill,
			switchEnabled:   true,
			switchReadErr:   errors.New("get configmap: connection refused"),
			targetNamespace: "k8s-buddy-plants",
			candidates:      []PodRef{inNS},
			wantKind:        ActionNone,
		},
		{
			name:            "empty candidate list is a no-op",
			mode:            ModePodKill,
			switchEnabled:   true,
			targetNamespace: "k8s-buddy-plants",
			candidates:      nil,
			wantKind:        ActionNone,
		},
		{
			name:            "out-of-namespace target refused",
			mode:            ModePodKill,
			switchEnabled:   true,
			targetNamespace: "k8s-buddy-plants",
			candidates:      []PodRef{outOfNS},
			wantKind:        ActionNone,
		},
		{
			name:            "pod-kill mode selects kill-pod action",
			mode:            ModePodKill,
			switchEnabled:   true,
			targetNamespace: "k8s-buddy-plants",
			candidates:      []PodRef{inNS},
			wantKind:        ActionKillPod,
			wantTarget:      inNS,
		},
		{
			name:            "readiness-flap mode selects flap-readiness action",
			mode:            ModeReadinessFlap,
			switchEnabled:   true,
			targetNamespace: "k8s-buddy-plants",
			candidates:      []PodRef{inNS},
			wantKind:        ActionFlapReadiness,
			wantTarget:      inNS,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := Decide(tc.mode, tc.switchEnabled, tc.switchReadErr, tc.targetNamespace, tc.candidates, rand.New(rand.NewSource(7)))
			assert.Equal(t, tc.wantKind, d.Kind)
			if tc.wantKind == ActionNone {
				assert.NotEmpty(t, d.Reason, "an ActionNone decision must explain itself")
			} else {
				assert.Equal(t, tc.wantTarget, d.Target)
				assert.Empty(t, d.Reason)
			}
		})
	}
}

// --- Execute -----------------------------------------------------------

func TestExecute_ActionNone_IsNoOp(t *testing.T) {
	fake := &fakePodClient{t: t}
	outcome, err := Execute(context.Background(), fake, Decision{Kind: ActionNone}, false)
	require.NoError(t, err)
	assert.Empty(t, outcome)
	assert.Empty(t, fake.events)
}

// TestExecute_DryRun_PerformsNoDestructiveCall is the load-bearing safety
// test: with dryRun=true, Execute must reach neither DeletePod nor
// FlapReadiness. fakePodClient.t is set, so either call fails the test
// immediately via t.Fatal rather than merely being detected after the
// fact by a bool check -- a regression here fails at the exact call site.
func TestExecute_DryRun_PerformsNoDestructiveCall(t *testing.T) {
	target := PodRef{Name: "fernie-1", Namespace: "k8s-buddy-plants", UID: "abc-123"}

	for _, kind := range []ActionKind{ActionKillPod, ActionFlapReadiness} {
		t.Run(string(kind), func(t *testing.T) {
			fake := &fakePodClient{t: t}
			outcome, err := Execute(context.Background(), fake, Decision{Kind: kind, Target: target}, true)
			require.NoError(t, err)
			assert.Equal(t, OutcomeDryRun, outcome)
			require.Len(t, fake.events, 1, "dry-run must still emit an event narrating the intended action")
			assert.Equal(t, corev1EventTypeNormal, fake.events[0].eventType)
			assert.Contains(t, fake.events[0].message, "dry-run")
		})
	}
}

func TestExecute_RealPodKill_CallsDeleteAndEmitsEvent(t *testing.T) {
	target := PodRef{Name: "fernie-1", Namespace: "k8s-buddy-plants", UID: "abc-123"}
	fake := &fakePodClient{}

	outcome, err := Execute(context.Background(), fake, Decision{Kind: ActionKillPod, Target: target}, false)

	require.NoError(t, err)
	assert.Equal(t, OutcomeSuccess, outcome)
	assert.True(t, fake.deleteCalled)
	assert.False(t, fake.flapCalled)
	require.Len(t, fake.events, 1)
	assert.Equal(t, "ChaosPodKill", fake.events[0].reason)
}

func TestExecute_RealReadinessFlap_CallsFlapAndEmitsEvent(t *testing.T) {
	target := PodRef{Name: "fernie-1", Namespace: "k8s-buddy-plants", UID: "abc-123", IP: "10.0.0.5"}
	fake := &fakePodClient{}

	outcome, err := Execute(context.Background(), fake, Decision{Kind: ActionFlapReadiness, Target: target}, false)

	require.NoError(t, err)
	assert.Equal(t, OutcomeSuccess, outcome)
	assert.True(t, fake.flapCalled)
	assert.False(t, fake.deleteCalled)
	require.Len(t, fake.events, 1)
	assert.Equal(t, "ChaosReadinessFlap", fake.events[0].reason)
}

func TestExecute_RealActionFailure_ReturnsFailureOutcomeAndEmitsWarningEvent(t *testing.T) {
	target := PodRef{Name: "fernie-1", Namespace: "k8s-buddy-plants"}
	fake := &fakePodClient{deleteErr: errors.New("pods \"fernie-1\" is forbidden")}

	outcome, err := Execute(context.Background(), fake, Decision{Kind: ActionKillPod, Target: target}, false)

	require.Error(t, err)
	assert.Equal(t, OutcomeFailure, outcome)
	require.Len(t, fake.events, 1)
	assert.Equal(t, corev1EventTypeWarning, fake.events[0].eventType)
}

// corev1EventTypeNormal and corev1EventTypeWarning mirror
// corev1.EventTypeNormal/EventTypeWarning's exact string values ("Normal",
// "Warning") without importing k8s.io/api/core/v1 into this test file --
// this package's stated design keeps engine.go and its tests importable
// without client-go or the Kubernetes API types, and duplicating two
// literal constants costs far less than reopening that boundary for a
// single assertion.
const (
	corev1EventTypeNormal  = "Normal"
	corev1EventTypeWarning = "Warning"
)
