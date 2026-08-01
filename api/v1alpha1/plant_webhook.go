// Package v1alpha1 pins the two admission webhook configuration objects
// below to explicit names (rather than accepting controller-gen's generic
// "mutating-webhook-configuration" default) for the same reason config/rbac's
// ClusterRole is generated with
// rbac:roleName=plant-operator-role instead of a generic default: they are
// cluster-scoped, so a generic name risks colliding with an unrelated
// operator's own webhook configuration on the same cluster. These exact
// names are also what internal/controller/plant_controller.go's own
// +kubebuilder:rbac resourceNames pins the caBundle-patching grant to, and
// what cmd/plant-operator's --mutating-webhook-configuration-name /
// --validating-webhook-configuration-name flags default to -- three
// independent places that must agree, kept in sync by convention like every
// other cross-file constant in this project (see plant_webhook.go's own
// minWateringInterval comment for the same pattern).
// +kubebuilder:webhookconfiguration:mutating=true,name=plant-operator-mutating-webhook-configuration
// +kubebuilder:webhookconfiguration:mutating=false,name=plant-operator-validating-webhook-configuration
// +kubebuilder:webhook:path=/mutate-buddy-k8s-buddy-io-v1alpha1-plant,mutating=true,failurePolicy=Ignore,sideEffects=None,groups=buddy.k8s-buddy.io,resources=plants,verbs=create;update,versions=v1alpha1,name=mplant-v1alpha1.kb.io,admissionReviewVersions=v1,timeoutSeconds=5,serviceName=plant-operator-webhook,serviceNamespace=k8s-buddy-system,servicePort=443
// +kubebuilder:webhook:path=/validate-buddy-k8s-buddy-io-v1alpha1-plant,mutating=false,failurePolicy=Fail,sideEffects=None,groups=buddy.k8s-buddy.io,resources=plants,verbs=create;update,versions=v1alpha1,name=vplant-v1alpha1.kb.io,admissionReviewVersions=v1,timeoutSeconds=5,serviceName=plant-operator-webhook,serviceNamespace=k8s-buddy-system,servicePort=443
package v1alpha1

// plant_webhook.go implements Plant's two admission webhooks using
// controller-runtime's decoupled webhook interfaces -- admission.Defaulter[T]
// and admission.Validator[T] -- rather than the deprecated webhook.Defaulter
// / webhook.Validator interfaces implemented directly on the CRD type itself
// (the old `func (r *Plant) Default()` / `func (r *Plant) ValidateCreate()`
// shape). A plain PlantCustomDefaulter{} / PlantCustomValidator{} struct
// implements them instead, which is what lets PlantCustomValidator carry
// configuration -- AllowedRegistries -- that has no business living on the
// API type, and keeps webhook logic out of plant_types.go entirely.
//
// controller-runtime v0.24 renamed this shape's own history mid-flight:
// admission.CustomDefaulter / admission.CustomValidator (T fixed to
// runtime.Object, with every method doing its own type assertion) are now
// the DEPRECATED spelling, superseded by the generic admission.Defaulter[T]
// / admission.Validator[T] this file uses instead (T = *Plant, so every
// method below is already concretely typed -- no assertion, no possible
// "expected a Plant but got a %T" branch to test). Both spellings implement
// the same decoupled-from-the-type design the task calls "the newer style";
// this file uses the one sigs.k8s.io/controller-runtime v0.24.1 (this
// project's pinned version, see go.mod) does not mark deprecated.
//
// The real request order, worth stating precisely because two comments
// below depend on it: CRD SCHEMA DEFAULTING runs at decode time -- before
// ANY admission plugin, mutating or validating, ever sees the object --
// then mutating admission webhooks run, then CRD structural/CEL VALIDATION
// runs (as part of the object strategy's own Validate, still before
// validating webhooks), then validating admission webhooks run. In short:
// default(decode) -> mutate(webhook) -> validate(schema) -> validate(webhook).
//
// The two webhooks this file registers:
//
//   - PlantCustomDefaulter: a MUTATING webhook filling every unset optional
//     PlantSpec field, agreeing field for field with the CRD's own
//     +kubebuilder:default markers (plant_types.go) -- see
//     plant_webhook_test.go's TestDefaultingAgreesWithCRDSchema, which reads
//     the generated CRD and fails the build the moment the two diverge,
//     rather than trusting two hand-maintained copies of the same six
//     values to stay in sync by convention. Given the real order above, this
//     webhook's own Default() is honestly, almost always, a NO-OP against
//     real traffic: CRD schema defaulting has already filled every unset
//     field by the time a mutating webhook ever runs. It still has to exist
//     and still has to agree exactly -- the task calls for a defaulting
//     webhook, and the one scenario schema-only defaulting cannot cover
//     (a Plant constructed directly against a Go client that skips the API
//     server's decode path entirely, e.g. inside a unit test building a
//     Plant object in memory) is exactly what TestDefaultingAgreesWithCRDSchema
//     and TestDefault_LeavesExplicitValuesAlone exercise directly, without a
//     real request in the loop at all.
//
//   - PlantCustomValidator: a VALIDATING webhook rejecting exactly what the
//     OpenAPI schema (and its CEL rules) cannot express: species immutability
//     on update, and an image registry outside an operator-configured
//     allowlist. It also re-asserts spec.replicas >= 1 and the
//     spec.wateringInterval floor as defense in depth -- both are already
//     enforced by the CRD schema itself (Minimum=1; the CEL rule at
//     30s/24h), so in the normal request path described above the CRD
//     rejects a bad value (at the validate(schema) step) before this webhook
//     ever sees it. See validate()'s own doc comment, and docs/adr/0009, for
//     why duplicating that check here is deliberate rather than dead code.
import (
	"context"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// The defaults every unset PlantSpec field resolves to. These MUST agree,
// value for value, with the +kubebuilder:default markers on the
// corresponding fields in plant_types.go -- that agreement is not a comment
// promising it, it is a compiled-in fact TestDefaultingAgreesWithCRDSchema
// checks mechanically against config/crd/bases/buddy.k8s-buddy.io_plants.yaml
// on every test run. Change a default here and forget plant_types.go (or
// vice versa) and that test fails; nothing here should ever be edited
// without also grepping for its twin.
const (
	// DefaultSpecies mirrors PlantSpec.Species's +kubebuilder:default=fern.
	DefaultSpecies = "fern"
	// DefaultReplicas mirrors PlantSpec.Replicas's +kubebuilder:default=3.
	DefaultReplicas int32 = 3
	// DefaultImage mirrors PlantSpec.Image's own +kubebuilder:default.
	DefaultImage = "ghcr.io/sean-kramer/k8s-buddy/buddy-api:dev"
	// DefaultResourceProfile mirrors PlantSpec.ResourceProfile's +kubebuilder:default=small.
	DefaultResourceProfile = "small"
	// DefaultWateringInterval mirrors PlantSpec.WateringInterval's
	// +kubebuilder:default="30s" -- and, not coincidentally, the CRD's own
	// CEL floor and the reconciler's minRequeueInterval
	// (internal/controller/plant_controller.go). All three are kept in sync
	// by convention, the same way suite_test.go's envtestK8sVersion is kept
	// in sync with the Makefile's ENVTEST_K8S_VERSION -- there is no single
	// source of truth shared across a Go const, a CRD YAML marker, and a CEL
	// expression string.
	DefaultWateringInterval = 30 * time.Second
	// DefaultLatencyBudget mirrors PlantSpec.LatencyBudget's +kubebuilder:default="150ms".
	DefaultLatencyBudget = 150 * time.Millisecond
	// DefaultChaosEnableEndpoints mirrors ChaosSpec.EnableEndpoints's
	// +kubebuilder:default=false. Included in the agreement test for
	// completeness even though a Go bool's own zero value already is false,
	// so this constant is never observably applied by Default() below -- see
	// that test's own comment for why it is still checked explicitly.
	DefaultChaosEnableEndpoints = false

	// minWateringInterval is this webhook's own copy of the reconciler's
	// requeue floor (minRequeueInterval, internal/controller/
	// plant_controller.go) and the CRD's CEL lower bound. api/v1alpha1 must
	// not import internal/controller (the dependency runs the other way), so
	// this is a second, independently-declared 30s -- kept in sync by the
	// same convention as DefaultWateringInterval above, not by a shared Go
	// symbol.
	minWateringInterval = 30 * time.Second

	// leafMessage is spec.replicas == 0's rejection message, used verbatim
	// and in full -- the project's own spec calls for exactly this string,
	// so it is a named constant rather than an inline literal to guarantee
	// every call site (and plant_webhook_test.go's assertion against it)
	// says precisely the same thing.
	leafMessage = "plants need at least one leaf"
)

// defaultAllowedImageRegistries is the allowlist PlantCustomValidator falls
// back to when the operator's own --allowed-image-registries flag/env var is
// left unset -- ghcr.io (this project's own registry) and Docker Hub's
// "library" namespace (official upstream images: postgres, redis, nginx,
// ...), which between them cover this project's own images and the most
// common demo/test dependencies without opening the allowlist to an
// arbitrary Docker Hub user account. An operator that wants a fully open
// allowlist must pass an explicit empty value -- see PlantCustomValidator's
// own doc comment.
var defaultAllowedImageRegistries = []string{"ghcr.io/", "docker.io/library/"}

// DefaultAllowedImageRegistries returns a fresh copy of
// defaultAllowedImageRegistries, for cmd/plant-operator's flag default and
// for tests -- a fresh slice each call so no caller can mutate the shared
// package-level default by holding a reference to it.
func DefaultAllowedImageRegistries() []string {
	out := make([]string, len(defaultAllowedImageRegistries))
	copy(out, defaultAllowedImageRegistries)
	return out
}

// PlantCustomDefaulter implements admission.Defaulter[*Plant]. It carries no
// configuration -- every default it applies is a fixed constant above -- so
// the zero value is always ready to use.
//
// +kubebuilder:object:generate=false
type PlantCustomDefaulter struct{}

var _ admission.Defaulter[*Plant] = &PlantCustomDefaulter{}

// Default fills every unset (zero-valued) optional field on plant's
// PlantSpec. "Unset" is judged the same way the CRD schema's own defaulting
// judges it -- Species/Image/ResourceProfile empty string, Replicas nil,
// WateringInterval/LatencyBudget zero Duration -- so a Plant that already
// carries an explicit value (including an explicit zero-length string,
// which the schema's MinLength markers reject before this ever runs) is
// never overwritten.
func (d *PlantCustomDefaulter) Default(_ context.Context, plant *Plant) error {
	if plant.Spec.Species == "" {
		plant.Spec.Species = DefaultSpecies
	}
	if plant.Spec.Replicas == nil {
		r := DefaultReplicas
		plant.Spec.Replicas = &r
	}
	if plant.Spec.Image == "" {
		plant.Spec.Image = DefaultImage
	}
	if plant.Spec.ResourceProfile == "" {
		plant.Spec.ResourceProfile = DefaultResourceProfile
	}
	if plant.Spec.WateringInterval.Duration == 0 {
		plant.Spec.WateringInterval = metav1.Duration{Duration: DefaultWateringInterval}
	}
	if plant.Spec.LatencyBudget.Duration == 0 {
		plant.Spec.LatencyBudget = metav1.Duration{Duration: DefaultLatencyBudget}
	}
	// ChaosSpec.EnableEndpoints needs no defaulting branch: its default
	// (false) and a Go bool's zero value are the same bit, so there is no
	// "unset" state to distinguish from "explicitly false" the way a pointer
	// or empty string would need.

	return nil
}

// PlantCustomValidator implements admission.Validator[*Plant].
// AllowedRegistries is the set of image registry prefixes spec.image is
// permitted to reference (see imageRegistryAllowed below for exactly how a
// prefix is matched) -- populated from cmd/plant-operator's
// --allowed-image-registries flag/env var, defaulting to
// DefaultAllowedImageRegistries() when neither is set. A nil or empty slice
// means "allow every registry": deliberately not the zero-value default (see
// SetupPlantWebhookWithManager), so an operator can only reach allow-all by
// setting the flag/env to an explicit empty string, never by omission.
//
// +kubebuilder:object:generate=false
type PlantCustomValidator struct {
	AllowedRegistries []string
}

var _ admission.Validator[*Plant] = &PlantCustomValidator{}

// ValidateCreate rejects a new Plant that violates any of this webhook's
// rules. It never inspects an "old" object -- there isn't one on create --
// so the immutability rule (species) is enforced only in ValidateUpdate,
// where an old object actually exists to compare against.
func (v *PlantCustomValidator) ValidateCreate(_ context.Context, plant *Plant) (admission.Warnings, error) {
	return nil, v.validate(plant)
}

// ValidateUpdate rejects an updated Plant that violates any of this
// webhook's create-time rules, PLUS the one rule that only makes sense on
// update: spec.species is immutable once set. CRD markers alone cannot
// express "this field may not change between two writes" -- an OpenAPI
// schema (and CEL's `self`) only ever sees one object at a time, never the
// stored old one -- so this is the clearest single demonstration of why a
// validating webhook exists at all in this project: it is the one rule nine
// pages of +kubebuilder markers on plant_types.go could not have written.
func (v *PlantCustomValidator) ValidateUpdate(_ context.Context, oldPlant, newPlant *Plant) (admission.Warnings, error) {
	if err := v.validate(newPlant); err != nil {
		return nil, err
	}

	if oldPlant.Spec.Species != newPlant.Spec.Species {
		return nil, fmt.Errorf(
			"spec.species is immutable: changing it from %q to %q is not allowed; "+
				"delete and recreate the Plant if it really is becoming a different species",
			oldPlant.Spec.Species, newPlant.Spec.Species)
	}

	return nil, nil
}

// ValidateDelete permits every delete unconditionally. A validating webhook
// this project ships MUST NOT be able to block deletion of a Plant -- see
// docs/adr/0009 for why the webhook's own admission-registration rules never
// list the DELETE verb in the first place, which makes this method dead code
// in practice; it is implemented anyway (returning nil, nil) purely to
// satisfy admission.Validator[*Plant], and as a second, redundant guarantee
// that a future rule addition here could never accidentally start rejecting
// deletes.
func (v *PlantCustomValidator) ValidateDelete(_ context.Context, _ *Plant) (admission.Warnings, error) {
	return nil, nil
}

// validate runs every create-time rule against plant and returns the first
// violation found, as a plain error (not a field.ErrorList wrapped in
// apierrors.NewInvalid): controller-runtime's admission handler renders a
// plain error's Error() string verbatim into the admission response's
// denial message (see (validatorForType).Handle in
// sigs.k8s.io/controller-runtime/pkg/webhook/admission/validator_custom.go,
// the `return Denied(err.Error())` branch), which is what lets the
// replicas==0 case return leafMessage EXACTLY, with no field-path prefix or
// wrapping text in front of it, matching this project's spec.
//
// Order matters only in that a Plant violating more than one rule reports
// just the first: replicas, then wateringInterval, then image. That is a
// deliberate simplicity choice (one message per rejected request, always
// human-readable) over aggregating every violation into one response.
func (v *PlantCustomValidator) validate(plant *Plant) error {
	if plant.Spec.Replicas != nil && *plant.Spec.Replicas == 0 {
		return fmt.Errorf("%s", leafMessage)
	}

	// Defense in depth, not the primary enforcement path -- see this file's
	// own header comment and docs/adr/0009. In the ordinary
	// default(decode)->mutate(webhook)->validate(schema)->validate(webhook)
	// request flow, the CRD's own CEL rule (plant_types.go's
	// XValidation:...duration(self) >= duration('30s')) rejects a sub-floor
	// value before this webhook ever runs. This check exists for the one
	// genuine way that CEL rule could stop being the thing that catches it:
	// a future schema change on plant_types.go that loosens or drops the
	// CEL rule without this webhook's own minWateringInterval being updated
	// in lockstep -- and for a friendlier message than CEL's own one-liner
	// on the rare chance it does still fire. (--validate=false is NOT such a
	// path: it is a client-side kubectl flag that only skips CLIENT-side
	// OpenAPI validation before the request is even sent -- the API
	// server's own structural/CEL validation always runs server-side
	// regardless of what any client requested.)
	if d := plant.Spec.WateringInterval.Duration; d > 0 && d < minWateringInterval {
		return fmt.Errorf(
			"spec.wateringInterval %s is below the operator's %s floor: "+
				"the reconciler will not requeue more often than that, so a shorter interval "+
				"would be silently clamped rather than honored -- set at least %s",
			d, minWateringInterval, minWateringInterval)
	}

	if plant.Spec.Image != "" && !imageRegistryAllowed(plant.Spec.Image, v.AllowedRegistries) {
		return fmt.Errorf(
			"spec.image %q is from a registry outside this operator's allowlist (%s); "+
				"ask a cluster admin to add it via --allowed-image-registries, or use an already-allowed image",
			plant.Spec.Image, strings.Join(v.AllowedRegistries, ", "))
	}

	return nil
}

// imageRegistryAllowed reports whether image's registry matches one of
// allowed's prefixes. An empty (including nil) allowed slice means allow
// every registry -- the explicit opt-out PlantCustomValidator's own doc
// comment describes -- so it always returns true in that case rather than
// rejecting everything by an accidentally-empty configuration.
func imageRegistryAllowed(image string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	normalized := normalizeImageRegistry(image)
	for _, prefix := range allowed {
		if prefix != "" && strings.HasPrefix(normalized, prefix) {
			return true
		}
	}
	return false
}

// normalizeImageRegistry rewrites image so it always begins with an explicit
// registry host, the same rule the Docker/OCI distribution spec's own
// reference parser uses to decide whether an image reference's first
// slash-separated component is a registry host or the start of a Docker Hub
// repository path: it is a host if and only if it contains a '.' or ':', or
// is exactly "localhost". Otherwise the reference is implicitly under
// Docker Hub ("docker.io"), with a bare, single-segment name ("nginx:latest")
// further defaulting to the "library/" namespace ("docker.io/library/nginx:latest")
// the same way `docker pull nginx` does.
//
// This exists so an allowlist entry like "docker.io/library/" matches
// `image: nginx:latest` even though the Plant's own spec.image never spells
// "docker.io" out -- an allowlist that only ever matched fully-qualified
// references would silently fail to protect the single most common way of
// writing an image reference at all.
func normalizeImageRegistry(image string) string {
	firstSlash := strings.Index(image, "/")
	if firstSlash == -1 {
		return "docker.io/library/" + image
	}

	firstComponent := image[:firstSlash]
	if strings.ContainsAny(firstComponent, ".:") || firstComponent == "localhost" {
		// Already carries an explicit registry host (a dot for a domain, a
		// colon for a host:port, or the "localhost" special case).
		return image
	}

	return "docker.io/" + image
}

// SetupPlantWebhookWithManager registers both of Plant's admission webhooks
// -- the mutating defaulter and the validating rule set above -- against
// mgr's webhook server, configuring the validator with allowedRegistries
// (see PlantCustomValidator's own doc comment for what a nil/empty slice
// means). cmd/plant-operator/main.go is this function's only caller.
//
// The +kubebuilder:webhook markers at the very top of this file (above the
// `package v1alpha1` clause -- controller-gen's webhook generator only
// collects this marker where it is a PACKAGE-level doc comment, never a
// function's) are controller-gen's source for config/webhook/manifests.yaml
// (MutatingWebhookConfiguration and ValidatingWebhookConfiguration) -- see
// `make manifests`. Their two failurePolicy choices are deliberately
// different, and the reasoning is spelled out in docs/adr/0009 rather than
// repeated here as a comment on every field: Ignore for the mutating webhook
// (the CRD's own +kubebuilder:default markers are a fully adequate fallback
// -- see this file's own header comment), Fail for the validating webhook (a
// validating webhook that fails open enforces nothing when it matters
// most). Neither rule lists the DELETE verb, which is what keeps an
// unreachable webhook from ever blocking `kubectl delete plant` -- see
// ValidateDelete's own comment.
func SetupPlantWebhookWithManager(mgr ctrl.Manager, allowedRegistries []string) error {
	if err := ctrl.NewWebhookManagedBy(mgr, &Plant{}).
		WithDefaulter(&PlantCustomDefaulter{}).
		WithValidator(&PlantCustomValidator{AllowedRegistries: allowedRegistries}).
		Complete(); err != nil {
		return fmt.Errorf("registering Plant webhooks: %w", err)
	}
	return nil
}
