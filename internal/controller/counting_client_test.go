//go:build envtest

// This file exists for exactly one reason, worth spelling out in full since
// it is the entire justification for the extra machinery below:
// resourceVersion is NOT sufficient to prove a reconciler makes zero writes
// on an unchanged object.
//
// If a mutate function zeroes a server-defaulted field -- forgets to copy a
// field forward, say -- controllerutil.CreateOrUpdate compares the object
// before and after that mutate function runs, sees a difference (the
// defaulted value versus zero), and issues an Update on every single
// reconcile pass. But the API server re-applies its own defaulting to the
// incoming object before it's ever written to etcd, so the bytes etcd
// actually stores come out identical to what was already there --
// resourceVersion never advances. A permanent, silent write storm running
// against the API server on every WateringInterval, forever, looks -- by
// the resourceVersion measure -- indistinguishable from a healthy, idle
// reconciler. (This is not hypothetical: it is exactly the shape of Critical
// Task 3's own review found, in the original formulation of this test, that
// resourceVersion-based assertions would have let straight through.)
//
// The only way to see the write itself, rather than inferring it from a
// side effect a defaulting bug can silently erase, is to count the actual
// client calls the reconciler makes. countingClient below decorates the
// manager's real client.Client, forwarding every call unchanged while
// additionally tallying Create/Update/Patch against the main object (and,
// separately, Update/Patch made through Status()), each bucketed by the
// object's GroupVersionKind.
package controller

import (
	"context"
	"sync"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// writeCounts tallies how many times each write verb was called against
// objects of a single GroupVersionKind.
type writeCounts struct {
	Create int
	Update int
	Patch  int
}

// total is the sum of every verb writeCounts tracks. The idempotence test
// doesn't care which specific verb the reconciler used on a steady-state
// pass, only whether it wrote anything at all -- this is what it checks is
// zero.
func (w writeCounts) total() int {
	return w.Create + w.Update + w.Patch
}

// countingClient decorates a real client.Client: every method it does not
// explicitly override (Get, List, Delete, DeleteAllOf, Apply, Scheme,
// RESTMapper, ...) passes straight through via the embedded client.Client.
// Create, Update, and Patch against the main object are tallied by GVK;
// Status() returns a wrapper that tallies Update/Patch against the
// subresource the same way, in a separate bucket.
//
// Safe for concurrent use: the manager reconciles on its own goroutine,
// entirely separate from whichever goroutine a test's assertions run on, so
// every counter access below is guarded by mu.
type countingClient struct {
	client.Client

	mu     sync.Mutex
	byGVK  map[schema.GroupVersionKind]writeCounts
	status map[schema.GroupVersionKind]writeCounts
}

// newCountingClient wraps inner, ready to count writes made through it. inner
// is normally the manager's own client (see suite_test.go), so the counts
// reflect exactly what the running reconciler does -- never a separate,
// unwatched client a test might otherwise be tempted to construct.
func newCountingClient(inner client.Client) *countingClient {
	return &countingClient{
		Client: inner,
		byGVK:  map[schema.GroupVersionKind]writeCounts{},
		status: map[schema.GroupVersionKind]writeCounts{},
	}
}

// gvkFor resolves obj's GroupVersionKind through the wrapped client's own
// scheme, the same way controller-runtime's typed client does internally --
// it works whether or not obj's TypeMeta happens to be populated, which for
// a typed object built via a Go struct literal (as every mutate* function in
// plant_controller.go does) it normally is not.
func (c *countingClient) gvkFor(obj runtime.Object) schema.GroupVersionKind {
	gvk, err := c.GroupVersionKindFor(obj)
	if err != nil {
		// Every object this operator ever writes is registered in the test
		// scheme (suite_test.go registers both the client-go and
		// buddyv1alpha1 schemes before the manager starts), so this should
		// never actually happen. Bucketing under a visibly-broken key
		// rather than panicking keeps a scheme-registration bug a failed
		// assertion instead of a suite-ending panic mid-reconcile.
		return schema.GroupVersionKind{Kind: "unresolved-gvk:" + err.Error()}
	}
	return gvk
}

func (c *countingClient) record(bucket map[schema.GroupVersionKind]writeCounts, obj runtime.Object, mutate func(*writeCounts)) {
	gvk := c.gvkFor(obj)
	c.mu.Lock()
	defer c.mu.Unlock()
	wc := bucket[gvk]
	mutate(&wc)
	bucket[gvk] = wc
}

func (c *countingClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	c.record(c.byGVK, obj, func(wc *writeCounts) { wc.Create++ })
	return c.Client.Create(ctx, obj, opts...)
}

func (c *countingClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	c.record(c.byGVK, obj, func(wc *writeCounts) { wc.Update++ })
	return c.Client.Update(ctx, obj, opts...)
}

func (c *countingClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	c.record(c.byGVK, obj, func(wc *writeCounts) { wc.Patch++ })
	return c.Client.Patch(ctx, obj, patch, opts...)
}

// Status returns a SubResourceWriter that tallies Update/Patch calls against
// this client's own status bucket, keyed by the GVK of the parent object
// (obj) passed to Update/Patch -- for this operator, always Plant, since
// plant_controller.go never writes any other object's status subresource.
func (c *countingClient) Status() client.SubResourceWriter {
	return &countingStatusWriter{parent: c, inner: c.Client.Status()}
}

// snapshot returns a copy of the main-object write counts recorded so far,
// bucketed by GroupVersionKind. Safe to call while a reconcile may be
// in-flight on the manager's goroutine.
func (c *countingClient) snapshot() map[schema.GroupVersionKind]writeCounts {
	c.mu.Lock()
	defer c.mu.Unlock()
	return copyCounts(c.byGVK)
}

// statusSnapshot is snapshot's counterpart for writes made through Status().
func (c *countingClient) statusSnapshot() map[schema.GroupVersionKind]writeCounts {
	c.mu.Lock()
	defer c.mu.Unlock()
	return copyCounts(c.status)
}

// reset clears every counter, so a test can establish a clean baseline
// immediately before the single reconcile it wants to measure.
func (c *countingClient) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byGVK = map[schema.GroupVersionKind]writeCounts{}
	c.status = map[schema.GroupVersionKind]writeCounts{}
}

func copyCounts(m map[schema.GroupVersionKind]writeCounts) map[schema.GroupVersionKind]writeCounts {
	out := make(map[schema.GroupVersionKind]writeCounts, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// countingStatusWriter decorates the SubResourceWriter Status() returns,
// counting Update and Patch calls into parent's status bucket. Create and
// Apply pass straight through uncounted: this operator's reconciler never
// calls either against a status subresource (status always already exists
// once the main object does, and Reconcile always uses Update, never server-
// side Apply), so there is nothing case 5's assertions need from them.
type countingStatusWriter struct {
	parent *countingClient
	inner  client.SubResourceWriter
}

func (w *countingStatusWriter) Create(ctx context.Context, obj client.Object, subResource client.Object, opts ...client.SubResourceCreateOption) error {
	return w.inner.Create(ctx, obj, subResource, opts...)
}

func (w *countingStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	w.parent.record(w.parent.status, obj, func(wc *writeCounts) { wc.Update++ })
	return w.inner.Update(ctx, obj, opts...)
}

func (w *countingStatusWriter) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	w.parent.record(w.parent.status, obj, func(wc *writeCounts) { wc.Patch++ })
	return w.inner.Patch(ctx, obj, patch, opts...)
}

func (w *countingStatusWriter) Apply(ctx context.Context, obj runtime.ApplyConfiguration, opts ...client.SubResourceApplyOption) error {
	return w.inner.Apply(ctx, obj, opts...)
}
