// webhookcerts.go is the self-signed certificate bootstrap
// docs/adr/0009-webhook-certificate-strategy.md commits to instead of
// cert-manager: at every process startup it generates a fresh CA and a
// serving certificate for the webhook server, writes them to an
// emptyDir-backed CertDir the container's own Deployment mounts, and patches
// the resulting CA's PEM bytes into the caBundle field of both
// MutatingWebhookConfiguration and ValidatingWebhookConfiguration objects
// this project ships (config/webhook/manifests.yaml, deployed via
// deploy/kustomize/operator or charts/k8s-buddy).
//
// Nothing here is persisted across restarts on purpose -- see the ADR for
// why an ephemeral, regenerate-every-start certificate is the right trade
// for this project rather than a genuine liability.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

// webhookCertificateValidity is deliberately LONG -- 10 years, far beyond
// any realistic lifetime for a container on this project's kind cluster --
// even though the certificate is regenerated at every process start anyway
// (a restart, a rollout, a rescheduled pod) and gains nothing from being
// individually long-lived in the ordinary case. A short validity was tried
// first and rejected: nothing in this process ever proactively regenerates
// the certificate before it is used again, so a short-lived cert on a pod
// that simply stays up (a kind demo cluster left running over a weekend is
// the concrete, not hypothetical, case) eventually serves an EXPIRED leaf.
// The API server's own TLS verification then fails, and with the validating
// webhook's failurePolicy: Fail, every Plant CREATE/UPDATE starts failing --
// silently, with no restart, no log line from this process (it is not the
// one rejecting the request), and no automatic recovery, until someone
// happens to notice and manually bounces the pod.
//
// A 10-year certificate turns "will this expire while the pod is up" from a
// real, timer-driven failure mode into one that is only theoretically
// possible -- and webhookCertExpiryCheck below is the backstop for even
// that: it reports this process unhealthy well before NotAfter, so the
// kubelet restarts it (minting a fresh certificate) rather than ever
// actually serving an expired one.
const webhookCertificateValidity = 87600 * time.Hour // 10 years

// webhookCertExpiryReadinessMargin is how long before the generated serving
// certificate's actual NotAfter this process starts reporting itself
// unhealthy on both /healthz and /readyz (see webhookCertExpiryCheck).
// Given webhookCertificateValidity's 10-year window, this should never
// realistically fire during this project's lifetime -- it exists purely as
// a backstop against the failure mode webhookCertificateValidity's own
// comment describes, not as a mechanism this project expects to exercise.
const webhookCertExpiryReadinessMargin = 24 * time.Hour

// generateWebhookServingCertificate creates a fresh, self-signed CA and a
// leaf serving certificate signed by it (valid for dnsNames), writes the
// leaf's cert+key to certDir as tls.crt/tls.key -- the exact filenames
// controller-runtime's webhook.Server (CertDir option) expects -- and
// returns the CA certificate's own PEM bytes (which the caller merges into
// both webhook configurations' caBundle, see mergeCABundle, so the API
// server trusts the leaf this CA just signed) and the leaf certificate's own
// NotAfter, which the caller wires into webhookCertExpiryCheck.
//
// ECDSA P-256 rather than RSA: this keypair is regenerated on every process
// start and thrown away on every restart, so there is no long-term key
// storage to justify RSA's larger, slower keys -- P-256 is the same curve
// controller-runtime's own certwatcher tests use, and is fast enough to
// generate on every pod start without measurable startup delay.
func generateWebhookServingCertificate(certDir string, dnsNames []string) (caPEM []byte, leafNotAfter time.Time, err error) {
	if len(dnsNames) == 0 {
		return nil, time.Time{}, fmt.Errorf("generateWebhookServingCertificate: no DNS names given")
	}

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("generating CA key: %w", err)
	}
	caSerial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("generating CA serial number: %w", err)
	}
	now := time.Now()
	caTemplate := &x509.Certificate{
		SerialNumber:          caSerial,
		Subject:               pkix.Name{CommonName: "plant-operator-webhook-ca", Organization: []string{"k8s-buddy"}},
		NotBefore:             now.Add(-5 * time.Minute), // small backdate so clock skew between pod and API server never rejects the cert as "not yet valid"
		NotAfter:              now.Add(webhookCertificateValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("self-signing CA certificate: %w", err)
	}
	caCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("parsing freshly-created CA certificate: %w", err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("generating serving key: %w", err)
	}
	leafSerial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("generating serving certificate serial number: %w", err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: leafSerial,
		Subject:      pkix.Name{CommonName: dnsNames[0], Organization: []string{"k8s-buddy"}},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     now.Add(webhookCertificateValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsNames,
	}
	// A literal "127.0.0.1"/"::1" IP SAN costs nothing to add and makes the
	// cert usable for a `kubectl port-forward` against the webhook Service
	// during local debugging, which the DNS SANs alone would not cover.
	leafTemplate.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}

	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("signing serving certificate: %w", err)
	}
	leafCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	leafKeyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("marshaling serving key: %w", err)
	}
	leafKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: leafKeyDER})

	if err := os.MkdirAll(certDir, 0o750); err != nil {
		return nil, time.Time{}, fmt.Errorf("creating cert dir %s: %w", certDir, err)
	}
	// 0o600: private key material, readable only by the process that just
	// wrote it (uid 65532, this container's own non-root user -- see
	// deploy/kustomize/operator/deployment.yaml's securityContext).
	if err := os.WriteFile(filepath.Join(certDir, "tls.crt"), leafCertPEM, 0o600); err != nil {
		return nil, time.Time{}, fmt.Errorf("writing tls.crt: %w", err)
	}
	if err := os.WriteFile(filepath.Join(certDir, "tls.key"), leafKeyPEM, 0o600); err != nil {
		return nil, time.Time{}, fmt.Errorf("writing tls.key: %w", err)
	}

	return caCertPEM, leafTemplate.NotAfter, nil
}

// webhookServiceDNSNames returns the DNS names a serving certificate for
// serviceName.namespace's ClusterIP Service must carry so every form a
// client might dial it by -- bare, namespaced, and fully-qualified cluster
// DNS -- validates. The API server always dials webhooks by the
// fully-qualified form, but the other two cost nothing to include and make
// the same certificate usable for a manual `curl`/port-forward test too.
func webhookServiceDNSNames(serviceName, namespace string) []string {
	return []string{
		serviceName,
		fmt.Sprintf("%s.%s", serviceName, namespace),
		fmt.Sprintf("%s.%s.svc", serviceName, namespace),
		fmt.Sprintf("%s.%s.svc.cluster.local", serviceName, namespace),
	}
}

// maxRetainedCAs bounds how many distinct CAs mergeCABundle keeps in a
// caBundle at once. Every process restart mints a brand-new, random ECDSA
// CA -- unlike a cert-manager-style setup, there is no persistent key
// identity for "the same CA" to be reused across restarts, so dedup-by-byte
// -equality (see mergeCABundle's own keepValid) does NOT bound growth on its
// own: three restarts of the very same replica produce three distinct CAs,
// each a permanently-valid trust anchor (webhookCertificateValidity is 10
// years) for a private key that no longer exists anywhere the moment that
// Pod exits. Left unbounded, a operator restarted daily would accumulate
// hundreds of live trust anchors over the project's lifetime -- an
// unbounded-growth problem AND a quietly widening trust surface, on the one
// field this whole design exists to keep tightly scoped.
//
// 3 is not arbitrary: it is exactly what the two legitimate reasons for
// more-than-one-CA (see mergeCABundle's own doc comment) need at once --
// one rolling update's old+new CA overlap (2) plus one full extra generation
// of margin for a second rollout landing before the first has fully settled,
// or a genuinely down replica lagging a step behind the others. A CA whose
// process has already exited has no ongoing reason to stay trusted; keeping
// only the most recent few (by NotBefore, newest first) is what "trust
// exists only as long as something still needs it" looks like here.
const maxRetainedCAs = 3

// mergeCABundle folds newCAPEM into existing (a webhook's current
// clientConfig.caBundle, zero or more concatenated PEM CERTIFICATE blocks)
// by APPENDING rather than overwriting: every still-valid certificate
// already in existing is kept, newCAPEM's own certificate is added, anything
// already expired is pruned, and -- see maxRetainedCAs' own comment -- only
// the maxRetainedCAs most recently-issued (by NotBefore) survive; older
// surviving entries are dropped even though they are not yet expired.
// Malformed or non-certificate PEM blocks are skipped rather than failing
// the whole patch -- a defensively tolerant read of a field only this
// process's own previous generations have ever written.
//
// Appending (before the cap trims it back down) is what makes both a
// rolling update AND running more than one replica safe, neither of which a
// plain overwrite would be:
//
//   - Rolling update: the new Pod's certificate bootstrap patches its own,
//     brand-new CA into the caBundle BEFORE it is marked Ready, while the
//     OLD Pod is still the Service's only endpoint and is still presenting
//     the OLD leaf certificate. Overwriting here would mean the API server
//     stops trusting the old (still-serving) certificate the instant the
//     new Pod's caBundle patch lands, well before traffic ever reaches the
//     new Pod -- every Plant write fails x509 verification for the whole
//     readiness window. Appending keeps the old CA in the bundle
//     throughout, so the still-serving old Pod stays trusted right up until
//     it stops serving.
//   - Multiple replicas: with no leader election gating the webhook server
//     (every replica serves admission traffic, not just the leader), each
//     replica generates and trusts only its OWN CA. An overwrite would mean
//     only the last replica to patch ends up trusted at all -- the API
//     server's Service-level load balancing has no idea which replica
//     issued which certificate, so roughly (n-1)/n of calls would fail TLS
//     verification against whichever replica isn't the most recent writer.
//     Appending means the caBundle accumulates every currently-running
//     replica's CA, and any of them can be dialed successfully.
func mergeCABundle(existing, newCAPEM []byte) []byte {
	now := time.Now()
	seen := make(map[string]bool)
	type entry struct {
		block *pem.Block
		cert  *x509.Certificate
	}
	var entries []entry

	keepValid := func(pemBytes []byte) {
		rest := pemBytes
		for {
			var block *pem.Block
			block, rest = pem.Decode(rest)
			if block == nil {
				return
			}
			if block.Type != "CERTIFICATE" {
				continue
			}
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				continue // skip: not a certificate this code can reason about, but not fatal to the patch
			}
			if !cert.NotAfter.After(now) {
				continue // pruned: already expired
			}
			key := string(block.Bytes)
			if seen[key] {
				continue // already present (e.g. this exact CA survived from a previous merge)
			}
			seen[key] = true
			entries = append(entries, entry{block: block, cert: cert})
		}
	}

	keepValid(existing)
	keepValid(newCAPEM)

	// Newest first, so truncating to maxRetainedCAs keeps the most
	// recently-issued CAs and drops the oldest -- a CA whose process has
	// already exited (which, for anything past position 0, this one might
	// well have) has no ongoing reason to remain trusted. NotBefore, not
	// NotAfter: every CA this process mints shares the same
	// webhookCertificateValidity, so NotAfter would not actually order them
	// by issuance recency the way NotBefore does.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].cert.NotBefore.After(entries[j].cert.NotBefore)
	})
	if len(entries) > maxRetainedCAs {
		entries = entries[:maxRetainedCAs]
	}

	var merged []byte
	for _, e := range entries {
		merged = append(merged, pem.EncodeToMemory(e.block)...)
	}
	return merged
}

// webhookCertExpiryCheck returns a healthz.Checker reporting unhealthy once
// leafNotAfter is within webhookCertExpiryReadinessMargin -- registered on
// BOTH /healthz (so a failing liveness probe actually restarts the
// container, minting a fresh certificate) and /readyz (so the Pod is pulled
// out of the webhook Service's endpoints as early as possible, before the
// liveness probe's own failureThreshold*periodSeconds finally triggers the
// restart). See webhookCertificateValidity's own comment for why this
// should never realistically fire in this project's lifetime; it exists as
// the backstop for the case that it does.
func webhookCertExpiryCheck(leafNotAfter time.Time) healthz.Checker {
	return func(_ *http.Request) error {
		if !time.Now().Add(webhookCertExpiryReadinessMargin).Before(leafNotAfter) {
			return fmt.Errorf(
				"webhook serving certificate expires at %s, within the %s safety margin -- a restart is required to mint a fresh one",
				leafNotAfter.Format(time.RFC3339), webhookCertExpiryReadinessMargin)
		}
		return nil
	}
}

// patchWebhookCABundles GETs the named MutatingWebhookConfiguration and
// ValidatingWebhookConfiguration, MERGES caPEM into every webhook entry's
// clientConfig.caBundle (see mergeCABundle -- this appends and prunes
// expired entries, it never simply overwrites), and writes each back with
// Update -- using a direct (uncached) client built straight from cfg, since
// this runs once
// at startup before the manager's cache has synced (or, for a manager that
// never watches these cluster-scoped types at all, ever would). Both
// objects must already exist (created by `make manifests` +
// kubectl/helm/kustomize before this operator's Deployment is ever rolled
// out) -- see docs/adr/0009 for why creating them here instead, rather than
// only patching, was rejected.
//
// It retries with a bounded backoff rather than failing outright on the
// first NotFound or Conflict: a fresh `kubectl apply -k
// deploy/kustomize/operator` applies every resource in one pass, but API
// server admission and object creation are not perfectly ordered relative to
// this container starting, so a brief "the webhook configuration doesn't
// exist yet" window is expected, not exceptional.
func patchWebhookCABundles(ctx context.Context, cfg *rest.Config, scheme *k8sruntime.Scheme, mutatingName, validatingName string, caPEM []byte) error {
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("building direct client for webhook caBundle patch: %w", err)
	}

	backoff := wait.Backoff{Duration: 2 * time.Second, Factor: 1.5, Steps: 10, Cap: 30 * time.Second}

	if err := patchOneWebhookCABundle(ctx, c, backoff, "MutatingWebhookConfiguration", mutatingName, caPEM, true); err != nil {
		return err
	}
	if err := patchOneWebhookCABundle(ctx, c, backoff, "ValidatingWebhookConfiguration", validatingName, caPEM, false); err != nil {
		return err
	}
	return nil
}

// patchOneWebhookCABundle is patchWebhookCABundles' per-object body, split
// out so the Mutating/Validating cases -- identical except for the Go type
// -- share one retry loop instead of two copy-pasted ones.
func patchOneWebhookCABundle(ctx context.Context, c client.Client, backoff wait.Backoff, kind, name string, caPEM []byte, mutating bool) error {
	return wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		var updateErr error
		if mutating {
			obj := &admissionregistrationv1.MutatingWebhookConfiguration{}
			if err := c.Get(ctx, types.NamespacedName{Name: name}, obj); err != nil {
				return retryableGetError(err, kind, name)
			}
			for i := range obj.Webhooks {
				obj.Webhooks[i].ClientConfig.CABundle = mergeCABundle(obj.Webhooks[i].ClientConfig.CABundle, caPEM)
			}
			updateErr = c.Update(ctx, obj)
		} else {
			obj := &admissionregistrationv1.ValidatingWebhookConfiguration{}
			if err := c.Get(ctx, types.NamespacedName{Name: name}, obj); err != nil {
				return retryableGetError(err, kind, name)
			}
			for i := range obj.Webhooks {
				obj.Webhooks[i].ClientConfig.CABundle = mergeCABundle(obj.Webhooks[i].ClientConfig.CABundle, caPEM)
			}
			updateErr = c.Update(ctx, obj)
		}
		if updateErr != nil {
			if apierrors.IsConflict(updateErr) {
				// Lost a race with something else writing the same object
				// (extremely unlikely -- nothing else in this project writes
				// these -- but Conflict is always safe to retry with a fresh
				// Get on the next iteration).
				return false, nil
			}
			return false, fmt.Errorf("patching caBundle onto %s/%s: %w", kind, name, updateErr)
		}
		return true, nil
	})
}

// retryableGetError classifies a Get failure for the ExponentialBackoff loop
// above: NotFound retries (the object may not have been created yet by the
// surrounding kustomize/helm apply), anything else is fatal immediately.
func retryableGetError(err error, kind, name string) (bool, error) {
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return false, fmt.Errorf("getting %s/%s: %w", kind, name, err)
}
