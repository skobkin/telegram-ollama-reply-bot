package statistics

import "sort"

// scoreBinCount is the resolution of the score histograms: 20 equal bins on
// the closed domain [0,1] (bin i = [i/20,(i+1)/20), the last bin includes
// 1.0). The .05-multiple edges cover every policy threshold (.30–.90) on bin
// boundaries, so gate-inclusive counts align with bin edges.
const scoreBinCount = 20

// scoreEdges are the score-histogram bin boundaries rendered in exports:
// 21 finite edges for 20 bins, the last bin [19/20, 1] inclusive.
var scoreEdges = func() []float64 {
	edges := make([]float64, scoreBinCount+1)
	for i := range edges {
		edges[i] = float64(i) / float64(scoreBinCount)
	}

	return edges
}()

// latencyEdges are the fixed finite latency bin boundaries in milliseconds.
// The terminal bin [lastEdge, ∞) is open-ended: no +Inf value is ever
// stored or serialized (Go's JSON encoder cannot represent it).
var latencyEdges = []float64{100, 250, 500, 1000, 2500, 5000, 10000, 30000, 60000, 120000}

// executionLatencyEdges extend the shared latency grid for whole-turn
// durations, which legitimately run longer than single requests.
var executionLatencyEdges = append(append([]float64(nil), latencyEdges...), 300000)

// scoreHistogram is a bounded [0,1] histogram with scoreBinCount bins. The
// value 1.0 lands in the last bin; values outside the domain are clamped.
type scoreHistogram struct {
	counts [scoreBinCount]uint64
}

// record bins one score in [0,1].
func (h *scoreHistogram) record(v float64) {
	switch {
	case v <= 0:
		h.counts[0]++
	case v >= 1:
		h.counts[scoreBinCount-1]++
	default:
		h.counts[int(v*float64(scoreBinCount))]++
	}
}

func (h *scoreHistogram) add(other scoreHistogram) {
	for i := range h.counts {
		h.counts[i] += other.counts[i]
	}
}

// latencyHistogram is a fixed-edge histogram whose terminal bin
// [lastEdge, ∞) is open-ended. Values below the first edge land in bin 0.
type latencyHistogram struct {
	edges  []float64
	counts []uint64 // len(edges)+1: bin i for i<len(edges) is [edges[i-1],edges[i]), terminal bin open
}

func newLatencyHistogram(edges []float64) latencyHistogram {
	return latencyHistogram{
		edges:  append([]float64(nil), edges...),
		counts: make([]uint64, len(edges)+1),
	}
}

// record bins one non-negative millisecond value. Values below the first
// edge land in bin 0; values at or beyond the last edge land in the
// open-ended terminal bin.
func (h *latencyHistogram) record(ms float64) {
	idx := sort.SearchFloat64s(h.edges, ms)
	if idx < len(h.edges) && ms == h.edges[idx] {
		// An exact edge belongs to the UPPER bin: [edge, next).
		idx++
	}
	h.counts[idx]++
}

func (h *latencyHistogram) add(other latencyHistogram) {
	for i := range h.counts {
		h.counts[i] += other.counts[i]
	}
}

func (h *latencyHistogram) total() uint64 {
	var total uint64
	for _, c := range h.counts {
		total += c
	}

	return total
}

// percentile returns the p-quantile (p in (0,1]) of the histogram by
// cumulative counts with linear interpolation inside the containing bin.
// A percentile falling into the open-ended terminal bin is reported as a
// lower bound (the terminal bin's lower edge); an empty histogram reports
// unavailable (ok=false).
func (h *latencyHistogram) percentile(p float64) (value float64, lowerBound bool, ok bool) {
	total := h.total()
	if total == 0 {
		return 0, false, false
	}

	target := p * float64(total)
	var cumulative uint64
	for bin, count := range h.counts {
		if count == 0 {
			continue
		}
		cumulative += count
		if float64(cumulative) < target {
			continue
		}

		// The p-quantile falls inside this bin.
		lower := 0.0
		if bin > 0 {
			lower = h.edges[bin-1]
		}
		if bin == len(h.edges) {
			// Open-ended terminal bin: only a lower bound is expressible.

			return lower, true, true
		}
		upper := h.edges[bin]
		fraction := (float64(cumulative) - target) / float64(count)

		return upper - fraction*(upper-lower), false, true
	}

	// Unreachable: total > 0 guarantees a containing bin.
	return 0, false, false
}
