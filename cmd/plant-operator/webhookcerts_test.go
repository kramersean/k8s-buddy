package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestGenerateWebhookServingCertificate(t *testing.T) {
	dir := t.TempDir()
	dnsNames := []string{"plant-operator-webhook", "plant-operator-webhook.k8s-buddy-system.svc"}

	caPEM, leafNotAfter, err := generateWebhookServingCertificate(dir, dnsNames)
	require.NoError(t, err)

	// The CA PEM decodes to a real, self-signed CA certificate.
	block, _ := pem.Decode(caPEM)
	require.NotNil(t, block)
	caCert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	require.True(t, caCert.IsCA)

	// tls.crt/tls.key exist where controller-runtime's webhook.Server expects them.
	crtPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")
	require.FileExists(t, crtPath)
	require.FileExists(t, keyPath)

	leafPEM, err := os.ReadFile(crtPath)
	require.NoError(t, err)
	leafBlock, _ := pem.Decode(leafPEM)
	require.NotNil(t, leafBlock)
	leafCert, err := x509.ParseCertificate(leafBlock.Bytes)
	require.NoError(t, err)
	require.Equal(t, dnsNames, leafCert.DNSNames)

	// The leaf is signed by the returned CA, which is the whole point --
	// the API server must be able to verify the leaf using only caBundle.
	require.NoError(t, leafCert.CheckSignatureFrom(caCert))

	// leafNotAfter, returned separately for webhookCertExpiryCheck, matches
	// what's actually on the certificate written to disk.
	require.WithinDuration(t, leafCert.NotAfter, leafNotAfter, time.Second)

	// webhookCertificateValidity is 10 years -- assert it is nowhere near
	// the old, incident-prone 24h window this replaced.
	require.Greater(t, time.Until(leafNotAfter), 9*365*24*time.Hour,
		"leaf certificate validity regressed toward a short-lived window -- see webhookCertificateValidity's own comment for why that is a real, timer-driven failure mode, not a style preference")
}

func TestGenerateWebhookServingCertificate_RejectsNoDNSNames(t *testing.T) {
	_, _, err := generateWebhookServingCertificate(t.TempDir(), nil)
	require.Error(t, err)
}

// --- mergeCABundle -----------------------------------------------------

// selfSignedTestCA returns a minimal self-signed CA certificate's own PEM
// bytes, expiring notAfter, for exercising mergeCABundle without pulling in
// the full generateWebhookServingCertificate machinery.
func selfSignedTestCA(t *testing.T, commonName string, notAfter time.Time) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             notAfter.Add(-24 * time.Hour),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func countCertsInBundle(t *testing.T, bundle []byte) int {
	t.Helper()
	n := 0
	rest := bundle
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return n
		}
		n++
	}
}

func TestMergeCABundle_AppendsRatherThanOverwrites(t *testing.T) {
	oldCA := selfSignedTestCA(t, "old-pod", time.Now().Add(10*365*24*time.Hour))
	newCA := selfSignedTestCA(t, "new-pod", time.Now().Add(10*365*24*time.Hour))

	merged := mergeCABundle(oldCA, newCA)

	require.Equal(t, 2, countCertsInBundle(t, merged),
		"a rolling update's new Pod must not evict the old Pod's still-valid CA -- doing so blocks admission for the entire readiness overlap window")
	require.Contains(t, string(merged), string(oldCA[:64]))
	require.Contains(t, string(merged), string(newCA[:64]))
}

func TestMergeCABundle_PrunesExpiredEntries(t *testing.T) {
	expiredCA := selfSignedTestCA(t, "long-gone-pod", time.Now().Add(-24*time.Hour))
	newCA := selfSignedTestCA(t, "current-pod", time.Now().Add(10*365*24*time.Hour))

	merged := mergeCABundle(expiredCA, newCA)

	require.Equal(t, 1, countCertsInBundle(t, merged))
	require.NotContains(t, string(merged), string(expiredCA[:64]))
}

func TestMergeCABundle_DeduplicatesIdenticalEntries(t *testing.T) {
	ca := selfSignedTestCA(t, "same-pod", time.Now().Add(10*365*24*time.Hour))

	merged := mergeCABundle(ca, ca)

	require.Equal(t, 1, countCertsInBundle(t, merged),
		"merging the same CA into itself (e.g. a retried patch) must not grow the bundle")
}

func TestMergeCABundle_EmptyExistingBundleJustReturnsTheNewCA(t *testing.T) {
	newCA := selfSignedTestCA(t, "first-ever-pod", time.Now().Add(10*365*24*time.Hour))

	merged := mergeCABundle(nil, newCA)

	require.Equal(t, 1, countCertsInBundle(t, merged))
}

func TestMergeCABundle_SkipsGarbageWithoutFailing(t *testing.T) {
	garbage := []byte("not a pem block at all")
	newCA := selfSignedTestCA(t, "current-pod", time.Now().Add(10*365*24*time.Hour))

	merged := mergeCABundle(garbage, newCA)

	require.Equal(t, 1, countCertsInBundle(t, merged))
}

// --- webhookCertExpiryCheck ---------------------------------------------

func TestWebhookCertExpiryCheck(t *testing.T) {
	t.Run("healthy well before expiry", func(t *testing.T) {
		check := webhookCertExpiryCheck(time.Now().Add(10 * 365 * 24 * time.Hour))
		require.NoError(t, check(nil))
	})

	t.Run("unhealthy once inside the safety margin", func(t *testing.T) {
		check := webhookCertExpiryCheck(time.Now().Add(webhookCertExpiryReadinessMargin - time.Minute))
		err := check(nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "safety margin")
	})

	t.Run("unhealthy once already expired", func(t *testing.T) {
		check := webhookCertExpiryCheck(time.Now().Add(-time.Hour))
		require.Error(t, check(nil))
	})
}
