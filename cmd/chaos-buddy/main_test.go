package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sean-kramer/k8s-buddy/internal/chaos"
)

// chaosEnvKeys is every environment variable loadConfig or parseDryRunFlag
// reads. clearChaosEnv unsets all of them before each test (and each
// subtest), so no test's result can depend on what the host environment,
// or an earlier t.Setenv call elsewhere in the process, happens to carry --
// mirrors cmd/buddy-api/main_test.go's own per-test env reset.
var chaosEnvKeys = []string{
	"CHAOS_TARGET_NAMESPACE", "CHAOS_LABEL_SELECTOR", "CHAOS_MODE",
	"CHAOS_INTERVAL", "CHAOS_DRY_RUN", "CHAOS_CONFIGMAP_NAME", "CHAOS_LOG_LEVEL",
}

func clearChaosEnv(t *testing.T) {
	t.Helper()
	for _, key := range chaosEnvKeys {
		t.Setenv(key, "")
	}
}

// setChaosEnv clears every chaos-buddy env var and then sets exactly the
// ones in env, so each test case starts from a known-empty baseline rather
// than inheriting whatever a previous subtest (or the outer test binary's
// environment) left behind.
func setChaosEnv(t *testing.T, env map[string]string) {
	t.Helper()
	clearChaosEnv(t)
	for k, v := range env {
		t.Setenv(k, v)
	}
}

// TestLoadConfig_RejectsInvalidValues is table-driven per invalid
// environment, asserting loadConfig fails with a clear, variable-naming
// error rather than silently substituting a default or a "match
// everything" fallback. This is the regression guard the code review
// asked for: loadConfig and parseDryRunFlag are where four of
// internal/chaos's safety properties (empty/unset selector, unsupported
// mode, unset target namespace, dry-run defaulting true) actually become
// startup refusals -- the pure helpers they call (chaos.ValidateLabelSelector,
// chaos.ParseMode) were already covered by internal/chaos/engine_test.go,
// but nothing previously proved loadConfig actually CALLS them, in the
// right order, on the right inputs. Reorder or drop a validation call here
// and every previously-existing test still passed; these must not.
func TestLoadConfig_RejectsInvalidValues(t *testing.T) {
	// validEnv is the smallest environment loadConfig accepts, reused as a
	// base by every case below so each one exercises exactly one invalid
	// value at a time.
	validEnv := map[string]string{
		"CHAOS_TARGET_NAMESPACE": "k8s-buddy-plants",
		"CHAOS_LABEL_SELECTOR":   "buddy.k8s-buddy.io/plant=fernie",
	}

	withOverride := func(overrides map[string]string) map[string]string {
		env := make(map[string]string, len(validEnv)+len(overrides))
		for k, v := range validEnv {
			env[k] = v
		}
		for k, v := range overrides {
			env[k] = v
		}
		return env
	}

	tests := []struct {
		name       string
		env        map[string]string
		wantErrHas []string
	}{
		{
			name: "CHAOS_TARGET_NAMESPACE unset",
			env: map[string]string{
				"CHAOS_LABEL_SELECTOR": "app=x",
			},
			wantErrHas: []string{"CHAOS_TARGET_NAMESPACE"},
		},
		{
			name: "CHAOS_TARGET_NAMESPACE whitespace",
			env: map[string]string{
				"CHAOS_TARGET_NAMESPACE": "   ",
				"CHAOS_LABEL_SELECTOR":   "app=x",
			},
			wantErrHas: []string{"CHAOS_TARGET_NAMESPACE"},
		},
		{
			name: "CHAOS_LABEL_SELECTOR unset",
			env: map[string]string{
				"CHAOS_TARGET_NAMESPACE": "k8s-buddy-plants",
			},
			wantErrHas: []string{"CHAOS_LABEL_SELECTOR"},
		},
		{
			name:       "CHAOS_LABEL_SELECTOR whitespace",
			env:        withOverride(map[string]string{"CHAOS_LABEL_SELECTOR": "   "}),
			wantErrHas: []string{"CHAOS_LABEL_SELECTOR"},
		},
		{
			// Non-empty but syntactically invalid: chaos.ValidateLabelSelector
			// only rejects empty/whitespace, so this proves loadConfig's own
			// additional labels.Parse check (added per code review, so a typo
			// fails fast at startup instead of failing identically on every
			// loop iteration forever).
			name:       "CHAOS_LABEL_SELECTOR syntactically invalid",
			env:        withOverride(map[string]string{"CHAOS_LABEL_SELECTOR": "===not a selector==="}),
			wantErrHas: []string{"CHAOS_LABEL_SELECTOR"},
		},
		{
			name:       "CHAOS_MODE=latency",
			env:        withOverride(map[string]string{"CHAOS_MODE": "latency"}),
			wantErrHas: []string{"pod-kill", "readiness-flap"},
		},
		{
			name:       "CHAOS_MODE=cpu-burn",
			env:        withOverride(map[string]string{"CHAOS_MODE": "cpu-burn"}),
			wantErrHas: []string{"pod-kill", "readiness-flap"},
		},
		{
			name:       "CHAOS_MODE=oom",
			env:        withOverride(map[string]string{"CHAOS_MODE": "oom"}),
			wantErrHas: []string{"pod-kill", "readiness-flap"},
		},
		{
			name:       "CHAOS_INTERVAL zero",
			env:        withOverride(map[string]string{"CHAOS_INTERVAL": "0s"}),
			wantErrHas: []string{"CHAOS_INTERVAL"},
		},
		{
			name:       "CHAOS_INTERVAL negative",
			env:        withOverride(map[string]string{"CHAOS_INTERVAL": "-5s"}),
			wantErrHas: []string{"CHAOS_INTERVAL"},
		},
		{
			name:       "CHAOS_INTERVAL malformed",
			env:        withOverride(map[string]string{"CHAOS_INTERVAL": "not-a-duration"}),
			wantErrHas: []string{"CHAOS_INTERVAL"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setChaosEnv(t, tt.env)

			_, err := loadConfig(true)
			require.Error(t, err)
			for _, want := range tt.wantErrHas {
				require.Contains(t, err.Error(), want)
			}
		})
	}
}

// TestLoadConfig_ModeDefaultsToPodKill asserts that leaving CHAOS_MODE
// entirely unset resolves to chaos.ModePodKill -- the one mode the shipped
// deploy/kustomize/chaos/configmap.yaml actually runs -- rather than an
// error or an empty Mode.
func TestLoadConfig_ModeDefaultsToPodKill(t *testing.T) {
	setChaosEnv(t, map[string]string{
		"CHAOS_TARGET_NAMESPACE": "k8s-buddy-plants",
		"CHAOS_LABEL_SELECTOR":   "buddy.k8s-buddy.io/plant=fernie",
		// CHAOS_MODE deliberately absent.
	})

	cfg, err := loadConfig(true)
	require.NoError(t, err)
	require.Equal(t, chaos.ModePodKill, cfg.mode)
}

// TestLoadConfig_Valid pins every field loadConfig resolves against a
// fully valid, fully explicit environment -- the "everything you specify
// is exactly what you get" counterpart to
// TestLoadConfig_RejectsInvalidValues's refusals.
func TestLoadConfig_Valid(t *testing.T) {
	setChaosEnv(t, map[string]string{
		"CHAOS_TARGET_NAMESPACE": "k8s-buddy-plants",
		"CHAOS_LABEL_SELECTOR":   "buddy.k8s-buddy.io/plant=stormy",
		"CHAOS_MODE":             "readiness-flap",
		"CHAOS_INTERVAL":         "45s",
		"CHAOS_CONFIGMAP_NAME":   "custom-switch",
		"CHAOS_LOG_LEVEL":        "debug",
	})

	cfg, err := loadConfig(false)
	require.NoError(t, err)

	require.Equal(t, "k8s-buddy-plants", cfg.targetNamespace)
	require.Equal(t, "buddy.k8s-buddy.io/plant=stormy", cfg.labelSelector)
	require.Equal(t, chaos.ModeReadinessFlap, cfg.mode)
	require.Equal(t, 45*time.Second, cfg.interval)
	require.False(t, cfg.dryRun, "loadConfig must carry through the dryRun value it was given, not recompute it")
	require.Equal(t, "custom-switch", cfg.configMapName)
	require.Equal(t, "debug", cfg.logLevel)
}

// TestLoadConfig_Defaults pins the exact shipped default values loadConfig
// falls back to when only the two required variables are set, mirroring
// cmd/buddy-api/main_test.go's TestLoadConfig_Defaults.
func TestLoadConfig_Defaults(t *testing.T) {
	setChaosEnv(t, map[string]string{
		"CHAOS_TARGET_NAMESPACE": "k8s-buddy-plants",
		"CHAOS_LABEL_SELECTOR":   "buddy.k8s-buddy.io/plant=fernie",
	})

	cfg, err := loadConfig(true)
	require.NoError(t, err)

	require.Equal(t, chaos.ModePodKill, cfg.mode)
	require.Equal(t, 30*time.Second, cfg.interval)
	require.Equal(t, "chaos-buddy-switch", cfg.configMapName)
	require.Equal(t, "info", cfg.logLevel)
}

// TestParseDryRunFlag covers the safe-default, override, and
// invalid-environment paths for --dry-run's env-seeded default: unset
// CHAOS_DRY_RUN must resolve true (the safe default an accidental deploy
// relies on), an explicit CHAOS_DRY_RUN=false must be honored, and an
// explicit --dry-run flag argument must win over either.
func TestParseDryRunFlag(t *testing.T) {
	tests := []struct {
		name    string
		env     string // "" (the zero value) means unset, matching t.Setenv(key, "")'s own treatment as unset throughout this file.
		args    []string
		want    bool
		wantErr bool
	}{
		{
			name: "CHAOS_DRY_RUN unset, no flag -> true (the safe default)",
			env:  "",
			want: true,
		},
		{
			name: "CHAOS_DRY_RUN=true, no flag -> true",
			env:  "true",
			want: true,
		},
		{
			name: "CHAOS_DRY_RUN=false, no flag -> false",
			env:  "false",
			want: false,
		},
		{
			name: "CHAOS_DRY_RUN=true, explicit --dry-run=false overrides",
			env:  "true",
			args: []string{"--dry-run=false"},
			want: false,
		},
		{
			name: "CHAOS_DRY_RUN=false, explicit --dry-run=true overrides",
			env:  "false",
			args: []string{"--dry-run=true"},
			want: true,
		},
		{
			name:    "invalid CHAOS_DRY_RUN is a startup error, not a silent fallback",
			env:     "maybe",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CHAOS_DRY_RUN", tt.env)

			got, err := parseDryRunFlag(tt.args)
			if tt.wantErr {
				require.Error(t, err)
				require.Contains(t, err.Error(), "CHAOS_DRY_RUN")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}
