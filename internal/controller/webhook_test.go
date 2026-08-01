//go:build envtest

// This file proves Plant's two admission webhooks (api/v1alpha1/
// plant_webhook.go) actually fire against a REAL kube-apiserver -- envtest's
// own, per suite_test.go's WebhookInstallOptions wiring -- rather than only
// being exercised as bare Go function calls the way
// api/v1alpha1/plant_webhook_test.go's unit tests do. A webhook registered
// with the wrong path, the wrong rules, an unreachable clientConfig, or a
// caBundle that doesn't verify would pass every one of those unit tests and
// still do nothing on a real cluster; only a test that goes through an
// actual AdmissionReview round trip can catch that class of bug.
package controller

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	buddyv1alpha1 "github.com/sean-kramer/k8s-buddy/api/v1alpha1"
)

// --- defaulting webhook ----------------------------------------------------

// TestWebhook_DefaultingFillsUnsetFields is the brief's own defaulting
// scenario: a Plant with only Species set comes back fully specified. It
// asserts against buddyv1alpha1's own exported Default* constants (not
// against a second copy of the numbers) so this test and
// TestDefaultingAgreesWithCRDSchema (api/v1alpha1/plant_webhook_test.go)
// together prove the same chain end to end: webhook constants == CRD schema
// defaults == what a real API server actually returns.
func TestWebhook_DefaultingFillsUnsetFields(t *testing.T) {
	ns := newTestNamespace(t)
	plant := &buddyv1alpha1.Plant{
		ObjectMeta: metav1.ObjectMeta{Name: "species-only", Namespace: ns},
		Spec: buddyv1alpha1.PlantSpec{
			Species: "cactus",
		},
	}
	createPlant(t, plant)

	got := &buddyv1alpha1.Plant{}
	require.NoError(t, testClient.Get(testCtx, client.ObjectKeyFromObject(plant), got))

	// The one field this Plant set explicitly is untouched -- a defaulting
	// webhook that overwrote it would be a far worse bug than a merely
	// absent one.
	require.Equal(t, "cactus", got.Spec.Species)

	require.NotNil(t, got.Spec.Replicas, "spec.replicas was left nil -- the defaulting webhook (or the CRD's own schema default) never ran")
	require.EqualValues(t, buddyv1alpha1.DefaultReplicas, *got.Spec.Replicas)
	require.Equal(t, buddyv1alpha1.DefaultImage, got.Spec.Image)
	require.Equal(t, buddyv1alpha1.DefaultResourceProfile, got.Spec.ResourceProfile)
	require.Equal(t, buddyv1alpha1.DefaultWateringInterval, got.Spec.WateringInterval.Duration)
	require.Equal(t, buddyv1alpha1.DefaultLatencyBudget, got.Spec.LatencyBudget.Duration)
	require.Equal(t, buddyv1alpha1.DefaultChaosEnableEndpoints, got.Spec.Chaos.EnableEndpoints)
}

// --- validating webhook: replicas -------------------------------------------

// TestWebhook_RejectsZeroReplicas is the brief's own exact-message case.
// Against a real API server, spec.replicas Minimum=1 (a structural schema
// rule) is evaluated BEFORE any validating webhook ever sees the request --
// see api/v1alpha1/plant_types.go's own comment on why its CEL rule (not
// only PlantCustomValidator) now also carries this exact message -- so this
// test asserts the message is present in the rejection, the same contract a
// human reading `kubectl apply`'s output relies on, rather than asserting
// which layer produced it.
func TestWebhook_RejectsZeroReplicas(t *testing.T) {
	ns := newTestNamespace(t)
	plant := newTestPlant(ns, "zero-replicas", 0)

	err := testClient.Create(testCtx, plant)
	require.Error(t, err)
	require.Contains(t, err.Error(), "plants need at least one leaf")
}

// --- validating webhook: wateringInterval -----------------------------------

// TestWebhook_RejectsSubFloorWateringInterval proves a sub-floor
// wateringInterval is rejected end to end. Like the replicas case above, the
// CRD's own CEL rule (plant_types.go) is what actually fires first against a
// real API server -- PlantCustomValidator's identical check is defense in
// depth, documented in that file's own comment -- so this asserts rejection
// and the presence of the floor duration, not which layer produced it.
func TestWebhook_RejectsSubFloorWateringInterval(t *testing.T) {
	ns := newTestNamespace(t)
	plant := newTestPlant(ns, "sub-floor-watering", 3)
	plant.Spec.WateringInterval = metav1.Duration{Duration: 5 * time.Second}

	err := testClient.Create(testCtx, plant)
	require.Error(t, err)
	require.Contains(t, err.Error(), "30s")
}

// --- validating webhook: image allowlist ------------------------------------

// TestWebhook_RejectsDisallowedImageRegistry is the one rejection in this
// file the CRD's OpenAPI schema genuinely cannot express at all (an
// operator-configured allowlist has no representation in a static schema),
// so unlike the two cases above, this rejection can ONLY come from
// PlantCustomValidator -- there is no schema layer to race against.
func TestWebhook_RejectsDisallowedImageRegistry(t *testing.T) {
	ns := newTestNamespace(t)
	plant := newTestPlant(ns, "disallowed-image", 3)
	plant.Spec.Image = "quay.io/someorg/buddy-api:dev"

	err := testClient.Create(testCtx, plant)
	require.Error(t, err)
	require.Contains(t, err.Error(), "quay.io/someorg/buddy-api:dev")
}

// TestWebhook_AllowsDefaultRegistry proves the allowlist is not
// accidentally rejecting everything: a Plant using this suite's own default
// image (ghcr.io/..., the same one buddyv1alpha1.DefaultAllowedImageRegistries
// permits) is admitted normally.
func TestWebhook_AllowsDefaultRegistry(t *testing.T) {
	ns := newTestNamespace(t)
	plant := newTestPlant(ns, "allowed-image", 3)
	createPlant(t, plant) // require.NoError inside createPlant -- this is the assertion.
}

// --- validating webhook: species immutability -------------------------------

// TestWebhook_SpeciesImmutable_RejectsChange_AllowsMutableFieldChange is the
// task brief's own pairing, deliberately exercised as ONE test against the
// SAME Plant: changing spec.species on update is rejected, naming both the
// old and new values, and a subsequent change to a genuinely mutable field
// on that same object SUCCEEDS. Splitting these into two independent tests
// would only prove "some update fails" and "some update succeeds" --
// asserting them back-to-back against one object is what actually proves
// the rejection is scoped to species specifically, not a webhook silently
// rejecting every update.
func TestWebhook_SpeciesImmutable_RejectsChange_AllowsMutableFieldChange(t *testing.T) {
	ns := newTestNamespace(t)
	plant := newTestPlant(ns, "immutable-species", 3)
	createPlant(t, plant)

	key := client.ObjectKeyFromObject(plant)

	fresh := &buddyv1alpha1.Plant{}
	require.NoError(t, testClient.Get(testCtx, key, fresh))
	fresh.Spec.Species = "cactus"
	err := testClient.Update(testCtx, fresh)
	require.Error(t, err)
	require.Contains(t, err.Error(), "fern")
	require.Contains(t, err.Error(), "cactus")
	require.Contains(t, err.Error(), "immutable")

	// The rejected update above must not have partially applied -- species
	// on the server is still "fern".
	unchanged := &buddyv1alpha1.Plant{}
	require.NoError(t, testClient.Get(testCtx, key, unchanged))
	require.Equal(t, "fern", unchanged.Spec.Species)

	// Now change a genuinely mutable field (replicas) on the SAME object and
	// prove it succeeds -- immutability is scoped to species, not a blanket
	// "no updates" rule.
	updated := updatePlant(t, key, func(p *buddyv1alpha1.Plant) {
		r := int32(5)
		p.Spec.Replicas = &r
	})
	require.EqualValues(t, 5, *updated.Spec.Replicas)
	require.Equal(t, "fern", updated.Spec.Species, "species must still be unchanged after the mutable-field update")
}
