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
	"os"
	"path/filepath"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// webhookCertificateValidity is deliberately short: this certificate is
// regenerated at every process start (a restart, a rollout, a rescheduled
// pod) and the caBundle is re-patched to match every time, so there is no
// scenario where a long-lived cert is needed to survive between
// regenerations the way a cert-manager-issued one would need to. A short
// validity window bounds how stale an un-patched caBundle could ever make a
// running pod's cert look, without buying anything in exchange.
const webhookCertificateValidity = 24 * time.Hour

// generateWebhookServingCertificate creates a fresh, self-signed CA and a
// leaf serving certificate signed by it (valid for dnsNames), writes the
// leaf's cert+key to certDir as tls.crt/tls.key -- the exact filenames
// controller-runtime's webhook.Server (CertDir option) expects -- and
// returns the CA certificate's own PEM bytes, which the caller patches into
// both webhook configurations' caBundle so the API server trusts the leaf
// this CA just signed.
//
// ECDSA P-256 rather than RSA: this keypair is regenerated on every process
// start and thrown away on every restart, so there is no long-term key
// storage to justify RSA's larger, slower keys -- P-256 is the same curve
// controller-runtime's own certwatcher tests use, and is fast enough to
// generate on every pod start without measurable startup delay.
func generateWebhookServingCertificate(certDir string, dnsNames []string) (caPEM []byte, err error) {
	if len(dnsNames) == 0 {
		return nil, fmt.Errorf("generateWebhookServingCertificate: no DNS names given")
	}

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating CA key: %w", err)
	}
	caSerial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generating CA serial number: %w", err)
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
		return nil, fmt.Errorf("self-signing CA certificate: %w", err)
	}
	caCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, fmt.Errorf("parsing freshly-created CA certificate: %w", err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating serving key: %w", err)
	}
	leafSerial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generating serving certificate serial number: %w", err)
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
		return nil, fmt.Errorf("signing serving certificate: %w", err)
	}
	leafCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	leafKeyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		return nil, fmt.Errorf("marshaling serving key: %w", err)
	}
	leafKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: leafKeyDER})

	if err := os.MkdirAll(certDir, 0o750); err != nil {
		return nil, fmt.Errorf("creating cert dir %s: %w", certDir, err)
	}
	// 0o600: private key material, readable only by the process that just
	// wrote it (uid 65532, this container's own non-root user -- see
	// deploy/kustomize/operator/deployment.yaml's securityContext).
	if err := os.WriteFile(filepath.Join(certDir, "tls.crt"), leafCertPEM, 0o600); err != nil {
		return nil, fmt.Errorf("writing tls.crt: %w", err)
	}
	if err := os.WriteFile(filepath.Join(certDir, "tls.key"), leafKeyPEM, 0o600); err != nil {
		return nil, fmt.Errorf("writing tls.key: %w", err)
	}

	return caCertPEM, nil
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

// patchWebhookCABundles GETs the named MutatingWebhookConfiguration and
// ValidatingWebhookConfiguration, sets every webhook entry's
// clientConfig.caBundle to caPEM, and writes each back with Update -- using
// a direct (uncached) client built straight from cfg, since this runs once
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
				obj.Webhooks[i].ClientConfig.CABundle = caPEM
			}
			updateErr = c.Update(ctx, obj)
		} else {
			obj := &admissionregistrationv1.ValidatingWebhookConfiguration{}
			if err := c.Get(ctx, types.NamespacedName{Name: name}, obj); err != nil {
				return retryableGetError(err, kind, name)
			}
			for i := range obj.Webhooks {
				obj.Webhooks[i].ClientConfig.CABundle = caPEM
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
