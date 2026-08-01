package v1alpha1

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// --- TestDefaultingAgreesWithCRDSchema ------------------------------------
//
// The mechanical agreement check the task brief asks for: rather than
// trusting that the constants in plant_webhook.go and the
// +kubebuilder:default markers in plant_types.go were typed identically by a
// human, this test reads the GENERATED CRD (config/crd/bases/
// buddy.k8s-buddy.io_plants.yaml -- the actual output `make manifests`
// produces from those markers, not a hand-copy of them) and compares its
// defaults against what PlantCustomDefaulter.Default actually writes onto an
// empty Plant. A future edit to one without the other -- a marker changed on
// plant_types.go with the webhook constant left alone, or vice versa --
// fails this test, because the two are compared as data, not asserted to be
// equal by two separate literals that happen to agree today.

// crdSchema is the minimal shape this test needs to read out of the
// generated CRD's spec.versions[0].schema.openAPIV3Schema.properties.spec --
// just enough structure to reach each field's own "default" value, however
// it is encoded (string, number, bool, or a nested object for "chaos").
type crdSchema struct {
	Spec struct {
		Versions []struct {
			Schema struct {
				OpenAPIV3Schema struct {
					Properties struct {
						Spec struct {
							Properties map[string]crdProperty `json:"properties"`
						} `json:"spec"`
					} `json:"properties"`
				} `json:"openAPIV3Schema"`
			} `json:"schema"`
		} `json:"versions"`
	} `json:"spec"`
}

type crdProperty struct {
	Default    any                    `json:"default"`
	Properties map[string]crdProperty `json:"properties"`
	// XKubernetesValidations is the field's own list of CEL rules (the
	// generated form of one or more +kubebuilder:validation:XValidation
	// markers), read by TestLeafMessageAgreesWithCRDSchema below.
	XKubernetesValidations []crdValidationRule `json:"x-kubernetes-validations"`
}

// crdValidationRule is one entry of a field's `x-kubernetes-validations`
// list: a CEL expression and the message the API server surfaces when it
// evaluates false.
type crdValidationRule struct {
	Rule    string `json:"rule"`
	Message string `json:"message"`
}

// loadPlantCRDSchema reads and parses the generated CRD, failing the test
// (not skipping it) if the file is missing -- exactly the same "fail loudly,
// never silently pass" posture suite_test.go's own resolveKubebuilderAssets
// takes on a missing envtest binary. A missing generated file here means
// `make manifests` was never run, which is itself the bug this test exists
// to catch one layer up from.
func loadPlantCRDSchema(t *testing.T) crdSchema {
	t.Helper()
	path := filepath.Join("..", "..", "config", "crd", "bases", "buddy.k8s-buddy.io_plants.yaml")
	raw, err := os.ReadFile(path)
	require.NoError(t, err, "reading generated CRD at %s -- run `make manifests` first", path)

	var doc crdSchema
	require.NoError(t, yaml.Unmarshal(raw, &doc), "parsing generated CRD at %s", path)
	require.NotEmpty(t, doc.Spec.Versions, "generated CRD at %s has no spec.versions", path)
	require.NotEmpty(t, doc.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties.Spec.Properties,
		"generated CRD at %s has no spec.versions[0].schema.openAPIV3Schema.properties.spec.properties", path)
	return doc
}

// crdDefault looks up field's declared +kubebuilder:default from the parsed
// CRD schema (spec.versions[0]...properties.spec.properties[field].default),
// failing the test outright if the field or its default is missing --
// exactly the failure mode that catches a default marker removed from
// plant_types.go without the corresponding webhook constant being removed
// too.
func crdDefault(t *testing.T, doc crdSchema, field string) any {
	t.Helper()
	props := doc.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties.Spec.Properties
	prop, ok := props[field]
	require.True(t, ok, "generated CRD has no spec.%s property at all", field)
	require.NotNil(t, prop.Default, "generated CRD's spec.%s carries no +kubebuilder:default", field)
	return prop.Default
}

// crdNestedDefault is crdDefault for a field nested one level down (only
// "chaos.enableEndpoints" needs this today).
func crdNestedDefault(t *testing.T, doc crdSchema, field, nested string) any {
	t.Helper()
	props := doc.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties.Spec.Properties
	prop, ok := props[field]
	require.True(t, ok, "generated CRD has no spec.%s property at all", field)
	nestedProp, ok := prop.Properties[nested]
	require.True(t, ok, "generated CRD has no spec.%s.%s property at all", field, nested)
	require.NotNil(t, nestedProp.Default, "generated CRD's spec.%s.%s carries no +kubebuilder:default", field, nested)
	return nestedProp.Default
}

// TestDefaultingAgreesWithCRDSchema is the agreement test itself: for every
// defaultable PlantSpec field, it applies PlantCustomDefaulter.Default to an
// otherwise-empty Plant and asserts the resulting value equals the CRD
// schema's own declared default for that exact field, read fresh off disk
// (see loadPlantCRDSchema) rather than off any constant this package
// declares. A webhook default that silently drifted from the CRD's own
// default -- the "genuine trap" the task brief warns about, where the value
// an admitted Plant actually gets depends on which layer happened to run --
// fails here.
func TestDefaultingAgreesWithCRDSchema(t *testing.T) {
	doc := loadPlantCRDSchema(t)

	plant := &Plant{}
	require.NoError(t, (&PlantCustomDefaulter{}).Default(context.Background(), plant))

	t.Run("species", func(t *testing.T) {
		require.Equal(t, crdDefault(t, doc, "species"), plant.Spec.Species)
		require.Equal(t, DefaultSpecies, plant.Spec.Species)
	})

	t.Run("replicas", func(t *testing.T) {
		want := crdDefault(t, doc, "replicas")
		// JSON/YAML numbers decode as float64 through `any`; the webhook's
		// own value is an int32 pointer. Comparing via float64 on both sides
		// is what makes this a genuine value comparison rather than a
		// type-shaped no-op.
		require.Equal(t, want, float64(*plant.Spec.Replicas))
		require.EqualValues(t, DefaultReplicas, *plant.Spec.Replicas)
	})

	t.Run("image", func(t *testing.T) {
		require.Equal(t, crdDefault(t, doc, "image"), plant.Spec.Image)
		require.Equal(t, DefaultImage, plant.Spec.Image)
	})

	t.Run("resourceProfile", func(t *testing.T) {
		require.Equal(t, crdDefault(t, doc, "resourceProfile"), plant.Spec.ResourceProfile)
		require.Equal(t, DefaultResourceProfile, plant.Spec.ResourceProfile)
	})

	t.Run("wateringInterval", func(t *testing.T) {
		want := crdDefault(t, doc, "wateringInterval")
		require.Equal(t, want, plant.Spec.WateringInterval.Duration.String())
		require.Equal(t, DefaultWateringInterval, plant.Spec.WateringInterval.Duration)
	})

	t.Run("latencyBudget", func(t *testing.T) {
		want := crdDefault(t, doc, "latencyBudget")
		require.Equal(t, want, plant.Spec.LatencyBudget.Duration.String())
		require.Equal(t, DefaultLatencyBudget, plant.Spec.LatencyBudget.Duration)
	})

	t.Run("chaos.enableEndpoints", func(t *testing.T) {
		want := crdNestedDefault(t, doc, "chaos", "enableEndpoints")
		require.Equal(t, want, plant.Spec.Chaos.EnableEndpoints)
		require.Equal(t, DefaultChaosEnableEndpoints, plant.Spec.Chaos.EnableEndpoints)
	})
}

// TestLeafMessageAgreesWithCRDSchema is TestDefaultingAgreesWithCRDSchema's
// counterpart for the OTHER hardcoded string this package and plant_types.go
// both carry: leafMessage (plant_webhook.go) and the additive CEL rule's own
// `message:` (plant_types.go's `+kubebuilder:validation:XValidation:rule="self
// >= 1",message="plants need at least one leaf"`) are two independent,
// hand-typed copies of the exact same sentence. Nothing forces them to stay
// identical except a human remembering to update both -- this test reads the
// generated CRD's own x-kubernetes-validations off disk (never a second
// hardcoded copy of the string in this test file) and fails the moment they
// diverge, the same mechanical guarantee TestDefaultingAgreesWithCRDSchema
// gives the six default values.
func TestLeafMessageAgreesWithCRDSchema(t *testing.T) {
	doc := loadPlantCRDSchema(t)
	props := doc.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties.Spec.Properties

	replicas, ok := props["replicas"]
	require.True(t, ok, "generated CRD has no spec.replicas property at all")
	require.NotEmpty(t, replicas.XKubernetesValidations,
		"generated CRD's spec.replicas carries no CEL rules -- expected the additive rule "+
			"backing the exact %q message (see plant_types.go's own comment on why it exists "+
			"alongside Minimum=1)", leafMessage)

	require.Equal(t, leafMessage, replicas.XKubernetesValidations[0].Message,
		"the CRD's own CEL message for spec.replicas must equal PlantCustomValidator's leafMessage "+
			"exactly -- these are two independently hardcoded copies of the same string "+
			"(plant_types.go's XValidation marker and plant_webhook.go's leafMessage const) that must "+
			"never diverge")
}

// TestDefault_LeavesExplicitValuesAlone proves the flip side of the
// agreement test above: a Plant that already carries explicit, non-default
// values is untouched by Default -- a defaulting webhook that overwrote an
// explicit value with the default would be a far worse bug than merely
// disagreeing with the CRD.
func TestDefault_LeavesExplicitValuesAlone(t *testing.T) {
	replicas := int32(7)
	plant := &Plant{
		Spec: PlantSpec{
			Species:          "cactus",
			Replicas:         &replicas,
			Image:            "ghcr.io/sean-kramer/k8s-buddy/buddy-api:v1.2.3",
			ResourceProfile:  "large",
			WateringInterval: metav1.Duration{Duration: 5 * time.Minute},
			LatencyBudget:    metav1.Duration{Duration: 300 * time.Millisecond},
			Chaos:            ChaosSpec{EnableEndpoints: true},
		},
	}
	want := plant.DeepCopy()

	require.NoError(t, (&PlantCustomDefaulter{}).Default(context.Background(), plant))

	require.Equal(t, want.Spec, plant.Spec)
}

// --- Validating webhook ----------------------------------------------------

func fullySpecifiedPlant() *Plant {
	replicas := int32(3)
	return &Plant{
		ObjectMeta: metav1.ObjectMeta{Name: "fernie", Namespace: "k8s-buddy-plants"},
		Spec: PlantSpec{
			Species:          "fern",
			Replicas:         &replicas,
			Image:            "ghcr.io/sean-kramer/k8s-buddy/buddy-api:dev",
			ResourceProfile:  "small",
			WateringInterval: metav1.Duration{Duration: 30 * time.Second},
			LatencyBudget:    metav1.Duration{Duration: 150 * time.Millisecond},
		},
	}
}

func TestValidateCreate_Accepts_FullySpecifiedPlant(t *testing.T) {
	v := &PlantCustomValidator{AllowedRegistries: DefaultAllowedImageRegistries()}
	warnings, err := v.ValidateCreate(context.Background(), fullySpecifiedPlant())
	require.NoError(t, err)
	require.Empty(t, warnings)
}

// TestValidateCreate_RejectsZeroReplicas is the exact-message assertion the
// task brief calls out by name: spec.replicas == 0 must be rejected with
// the string "plants need at least one leaf" and NOTHING else appended --
// no field path prefix, no wrapping sentence.
func TestValidateCreate_RejectsZeroReplicas(t *testing.T) {
	v := &PlantCustomValidator{AllowedRegistries: DefaultAllowedImageRegistries()}
	plant := fullySpecifiedPlant()
	zero := int32(0)
	plant.Spec.Replicas = &zero

	_, err := v.ValidateCreate(context.Background(), plant)
	require.Error(t, err)
	require.Equal(t, "plants need at least one leaf", err.Error())
}

func TestValidateCreate_RejectsSubFloorWateringInterval(t *testing.T) {
	v := &PlantCustomValidator{AllowedRegistries: DefaultAllowedImageRegistries()}
	plant := fullySpecifiedPlant()
	plant.Spec.WateringInterval = metav1.Duration{Duration: 5 * time.Second}

	_, err := v.ValidateCreate(context.Background(), plant)
	require.Error(t, err)
	require.Contains(t, err.Error(), "30s")
	require.Contains(t, err.Error(), "floor")
}

func TestValidateCreate_AcceptsExactlyFloorWateringInterval(t *testing.T) {
	v := &PlantCustomValidator{AllowedRegistries: DefaultAllowedImageRegistries()}
	plant := fullySpecifiedPlant()
	plant.Spec.WateringInterval = metav1.Duration{Duration: minWateringInterval}

	_, err := v.ValidateCreate(context.Background(), plant)
	require.NoError(t, err)
}

func TestValidateCreate_RejectsDisallowedRegistry(t *testing.T) {
	v := &PlantCustomValidator{AllowedRegistries: []string{"ghcr.io/", "docker.io/library/"}}
	plant := fullySpecifiedPlant()
	plant.Spec.Image = "quay.io/someorg/buddy-api:dev"

	_, err := v.ValidateCreate(context.Background(), plant)
	require.Error(t, err)
	require.Contains(t, err.Error(), "quay.io/someorg/buddy-api:dev")
}

func TestValidateCreate_EmptyAllowlistAllowsEverything(t *testing.T) {
	v := &PlantCustomValidator{AllowedRegistries: nil}
	plant := fullySpecifiedPlant()
	plant.Spec.Image = "quay.io/someorg/buddy-api:dev"

	_, err := v.ValidateCreate(context.Background(), plant)
	require.NoError(t, err)
}

func TestImageRegistryAllowed(t *testing.T) {
	allowed := []string{"ghcr.io/", "docker.io/library/"}

	cases := []struct {
		name  string
		image string
		want  bool
	}{
		{"ghcr image", "ghcr.io/sean-kramer/k8s-buddy/buddy-api:dev", true},
		{"bare docker hub library image", "nginx:latest", true},
		{"bare docker hub library image, no tag", "redis", true},
		{"docker hub non-library org", "someuser/repo:tag", false},
		{"explicit docker.io non-library", "docker.io/someuser/repo:tag", false},
		{"localhost registry", "localhost:5000/buddy-api:dev", false},
		{"disallowed third-party registry", "quay.io/someorg/buddy-api:dev", false},
		{"ghcr with digest", "ghcr.io/sean-kramer/k8s-buddy/buddy-api@sha256:" + fortyTwoHexChars(), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, imageRegistryAllowed(tc.image, allowed))
		})
	}
}

func fortyTwoHexChars() string {
	// A syntactically-plausible-looking sha256 hex string; imageRegistryAllowed
	// never actually validates digest shape (the CRD's own Pattern marker
	// does that), so any 64 hex characters exercise the code path the same way.
	s := ""
	for i := 0; i < 64; i++ {
		s += "a"
	}
	return s
}

func TestValidateUpdate_Accepts_MutableFieldChange(t *testing.T) {
	v := &PlantCustomValidator{AllowedRegistries: DefaultAllowedImageRegistries()}
	oldPlant := fullySpecifiedPlant()
	newPlant := oldPlant.DeepCopy()
	newReplicas := int32(5)
	newPlant.Spec.Replicas = &newReplicas

	_, err := v.ValidateUpdate(context.Background(), oldPlant, newPlant)
	require.NoError(t, err)
}

// TestValidateUpdate_RejectsSpeciesChange is the immutability demonstration
// the task brief calls the clearest reason a validating webhook exists at
// all: CRD markers alone cannot express "reject this field changing between
// two writes", so this rule lives here and nowhere else.
func TestValidateUpdate_RejectsSpeciesChange(t *testing.T) {
	v := &PlantCustomValidator{AllowedRegistries: DefaultAllowedImageRegistries()}
	oldPlant := fullySpecifiedPlant()
	newPlant := oldPlant.DeepCopy()
	newPlant.Spec.Species = "cactus"

	_, err := v.ValidateUpdate(context.Background(), oldPlant, newPlant)
	require.Error(t, err)
	require.Contains(t, err.Error(), "fern")
	require.Contains(t, err.Error(), "cactus")
	require.Contains(t, err.Error(), "immutable")
}

func TestValidateDelete_AlwaysAllowed(t *testing.T) {
	v := &PlantCustomValidator{AllowedRegistries: []string{"ghcr.io/"}}
	// Even a Plant that would fail every create-time rule must still be
	// deletable -- see ValidateDelete's own comment on why this method does
	// not call validate() at all.
	zero := int32(0)
	plant := fullySpecifiedPlant()
	plant.Spec.Replicas = &zero
	plant.Spec.Image = "quay.io/not-allowed:dev"

	_, err := v.ValidateDelete(context.Background(), plant)
	require.NoError(t, err)
}

func TestNormalizeImageRegistry(t *testing.T) {
	cases := map[string]string{
		"nginx":                         "docker.io/library/nginx",
		"nginx:latest":                  "docker.io/library/nginx:latest",
		"someuser/repo:tag":             "docker.io/someuser/repo:tag",
		"ghcr.io/org/repo:tag":          "ghcr.io/org/repo:tag",
		"localhost:5000/repo:tag":       "localhost:5000/repo:tag",
		"localhost/repo:tag":            "localhost/repo:tag",
		"registry.example.com/repo:tag": "registry.example.com/repo:tag",
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			require.Equal(t, want, normalizeImageRegistry(in))
		})
	}
}

func TestDefaultAllowedImageRegistries_ReturnsIndependentCopy(t *testing.T) {
	a := DefaultAllowedImageRegistries()
	a[0] = "mutated"
	b := DefaultAllowedImageRegistries()
	require.Equal(t, "ghcr.io/", b[0])
}
