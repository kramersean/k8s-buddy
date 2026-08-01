// This test file lives in package controller (not controller_test) so it can
// call the unexported requeueIntervalFor directly. It carries no build tag:
// the clamp it exercises is pure arithmetic and needs no control plane, and
// the reason it exists as a separate, unexported-facing test is that the CRD
// schema now REJECTS the pathological inputs at admission -- so the envtest
// suite structurally cannot construct a Plant that reaches this clamp.
// Testing the second line of defence requires bypassing the first.
package controller

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	buddyv1alpha1 "github.com/sean-kramer/k8s-buddy/api/v1alpha1"
)

// TestRequeueIntervalFor covers both failure modes minRequeueInterval guards,
// and the boundary between them.
//
// The clamp used to read `if requeueAfter <= 0`, which caught only the first:
// a Plant with wateringInterval 1ms was a valid object that pinned a
// reconcile worker in a 1ms loop against the API server for as long as it
// existed. `<` catches both.
func TestRequeueIntervalFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		interval time.Duration
		want     time.Duration
	}{
		// Never defaulted at all -- a Plant constructed directly in Go.
		// RequeueAfter: 0 means "no timer requeue", i.e. a Plant that
		// quietly stops being watered rather than failing loudly.
		{"zero is floored", 0, minRequeueInterval},
		{"negative is floored", -5 * time.Second, minRequeueInterval},

		// The busy loop. Each of these was previously returned verbatim.
		{"one nanosecond is floored", time.Nanosecond, minRequeueInterval},
		{"one millisecond is floored", time.Millisecond, minRequeueInterval},
		{"one second is floored", time.Second, minRequeueInterval},
		{"just under the floor is floored", minRequeueInterval - time.Nanosecond, minRequeueInterval},

		// The boundary must be inclusive: exactly 30s is the documented
		// default and must survive the clamp unchanged.
		{"exactly the floor is unchanged", minRequeueInterval, minRequeueInterval},

		// Anything above the floor is the user's own choice and is honored.
		{"above the floor is unchanged", 5 * time.Minute, 5 * time.Minute},
		{"well above the floor is unchanged", 24 * time.Hour, 24 * time.Hour},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plant := &buddyv1alpha1.Plant{
				Spec: buddyv1alpha1.PlantSpec{
					WateringInterval: metav1.Duration{Duration: tc.interval},
				},
			}
			require.Equal(t, tc.want, requeueIntervalFor(plant))
		})
	}
}

// TestRequeueIntervalFor_NeverBusyLoops is the property the table above
// encodes, stated once as an invariant rather than as a list of examples: no
// input, however hostile, can make this function return a delay short enough
// to constitute a busy loop.
func TestRequeueIntervalFor_NeverBusyLoops(t *testing.T) {
	t.Parallel()

	hostile := []time.Duration{
		-time.Hour, -1, 0, 1, time.Microsecond, time.Millisecond,
		100 * time.Millisecond, time.Second, 29 * time.Second,
	}
	for _, d := range hostile {
		plant := &buddyv1alpha1.Plant{
			Spec: buddyv1alpha1.PlantSpec{WateringInterval: metav1.Duration{Duration: d}},
		}
		got := requeueIntervalFor(plant)
		require.GreaterOrEqual(t, got, minRequeueInterval,
			"wateringInterval %s produced a requeue delay of %s, below the %s floor", d, got, minRequeueInterval)
	}
}
