package api

import (
	"math"
	"slices"
	"sync"
	"time"
)

// workWindowSize is how many of the most recent /work observations
// workWindow keeps. 100 is small enough that the whole window is a single
// ~1.6KB fixed array with no allocation and no eviction bookkeeping, and
// large enough that a p95 has 5 samples above it to be a percentile of
// rather than being a synonym for "the slowest request ever seen".
const workWindowSize = 100

// workObservation is one recorded /work request: how long it took, and
// whether it failed.
type workObservation struct {
	duration time.Duration
	failed   bool
}

// workWindow is a bounded, mutex-guarded ring buffer of the most recent
// workWindowSize /work observations. It exists because the mood engine
// needs live ErrorRate and P95Latency signals, and both of those are
// meaningless as lifetime totals: a pod that served a million clean
// requests last week and is failing every request right now would still
// report a near-zero lifetime error rate and score as perfectly healthy.
// A rolling window is what makes the mood reflect *now*.
//
// The buffer is a fixed array, never a growing slice: observe overwrites
// the oldest entry in place, so memory is constant regardless of how many
// requests the process serves, and an observation costs one store plus two
// integer updates -- no allocation, no eviction scan, no time comparison.
//
// A plain sync.Mutex guards it rather than atomics: unlike the single-word
// readiness flag, an observation mutates three fields that must move
// together, and a reader must see a self-consistent snapshot of all of
// them at once.
//
// The zero value is ready to use and reports an empty window.
type workWindow struct {
	mu    sync.Mutex
	buf   [workWindowSize]workObservation
	next  int
	count int
}

// observe records one /work request. Once the window is full, this
// overwrites the oldest observation, so the window never grows.
func (w *workWindow) observe(d time.Duration, failed bool) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.buf[w.next] = workObservation{duration: d, failed: failed}
	w.next = (w.next + 1) % workWindowSize
	if w.count < workWindowSize {
		w.count++
	}
}

// stats returns the error rate (failed observations over total, in [0,1])
// and the 95th-percentile duration across the window, plus how many
// observations that was computed from.
//
// An EMPTY window returns (0, 0, 0) -- explicitly, not as an accident of
// arithmetic. Those are the values that make a process which has not yet
// served any /work report as healthy rather than as either a division by
// zero or a NaN error rate that would poison the health score. "No
// evidence of trouble" is the correct reading of "no data" for a freshly
// started pod.
//
// The percentile is nearest-rank: sort ascending, take the element at
// ceil(0.95*n). No interpolation, so the value returned is always one an
// actual request really took. Sorting happens on a copy taken under the
// lock, so the lock is held for O(n) copying rather than O(n log n)
// sorting, and /work never waits on a /status caller's sort.
//
// Entries 0..count-1 are always the valid ones: before the window fills,
// next has only ever advanced past those indices; after it fills, every
// index holds a live observation. Order within the buffer is irrelevant
// here because both outputs are order-independent.
func (w *workWindow) stats() (errorRate float64, p95 time.Duration, n int) {
	var (
		durations [workWindowSize]time.Duration
		failures  int
	)

	w.mu.Lock()
	n = w.count
	for i := range n {
		obs := w.buf[i]
		durations[i] = obs.duration
		if obs.failed {
			failures++
		}
	}
	w.mu.Unlock()

	if n == 0 {
		return 0, 0, 0
	}

	sample := durations[:n]
	slices.Sort(sample)

	rank := int(math.Ceil(0.95*float64(n))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank > n-1 {
		rank = n - 1
	}

	return float64(failures) / float64(n), sample[rank], n
}
