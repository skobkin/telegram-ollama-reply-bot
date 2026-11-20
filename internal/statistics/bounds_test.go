package statistics

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// Serialization assumptions behind the documented worst-case sizes. Every
// number below is derived from the real collection constants, never typed in
// by hand, so a changed cap changes the bound (and the test fails loudly if a
// payload no longer fits the write limit).
const (
	// boundCounterBytes is the strict serialized width of one counter: 20
	// digits (uint64 max) plus a separator.
	boundCounterBytes = 21
	// boundBucketMetaBytes covers one bucket's JSON field names, dictionary
	// references and framing.
	boundBucketMetaBytes = 256
	// boundSlackBytes covers dictionaries, per-chat ledgers and totals.
	boundSlackBytes = 64 << 10
)

type interactionBound struct {
	decisionBuckets, executionBuckets int
	decisionCounters, execCounters    int
	payloadBytes                      int
	counterBytesRAM                   int
}

func interactionWorstCase() interactionBound {
	chats := MaxTrackedChats + 1
	profiles := MaxTrackedProfiles + 1
	models := MaxTrackedModels + 1

	b := interactionBound{
		decisionBuckets:  chats * profiles * models,
		executionBuckets: chats * profiles * models * models,
		decisionCounters: int(outcomeCount) + int(inferenceClassCount) + int(scoreDimCount)*scoreBinCount + 2*(len(latencyEdges)+1),
		execCounters:     int(actionCount) + 7 + int(execFailureCount-1) + len(executionLatencyEdges) + 1,
	}
	b.payloadBytes = b.decisionBuckets*(b.decisionCounters*boundCounterBytes+boundBucketMetaBytes) +
		b.executionBuckets*(b.execCounters*boundCounterBytes+boundBucketMetaBytes) +
		2*models*(maxModelIDBytes+4) + profiles*(maxProfileIDBytes+4) + chats*24 + boundSlackBytes
	b.counterBytesRAM = (b.decisionBuckets*b.decisionCounters + b.executionBuckets*b.execCounters) * 8

	return b
}

func saturateHistogram(h *latencyHistogram) {
	for i := range h.counts {
		h.counts[i] = math.MaxUint64
	}
}

// TestInteractionWorstCasePayloadFitsTheWriteLimit saturates every dimension
// (all caps plus the overflow keys) with maximum-width counters and checks the
// real payload against the derived bound and the write limit.
func TestInteractionWorstCasePayloadFitsTheWriteLimit(t *testing.T) {
	c := newPersistTestCollector()
	long := func(prefix string, i int) string {
		return prefix + fmt.Sprint(i) + strings.Repeat("x", maxModelIDBytes-len(prefix)-3)
	}

	for chat := int64(1); chat <= MaxTrackedChats+8; chat++ {
		epoch := c.BeginSubmission(chat)
		for p := 0; p < MaxTrackedProfiles+2; p++ {
			profile := fmt.Sprintf("p%d", p)
			for dm := 0; dm < MaxTrackedModels+2; dm++ {
				keys := ik(chat, profile, long("d", dm))
				c.RecordDecision(DecisionObservation{Epoch: epoch, Keys: keys, Outcome: OutcomeSelectedReply, Scores: scores(0.5), DecisionLatency: time.Second})
				c.RecordInference(InferenceAttempt{Epoch: epoch, Keys: keys, Latency: time.Second, ErrClass: InferenceSuccess})
				for gm := 0; gm < MaxTrackedModels+2; gm++ {
					c.RecordExecutionDispatch(ExecutionDispatch{Epoch: epoch, Keys: keys, GenerationModel: long("g", gm), Action: ActionReply})
				}
			}
		}
	}

	// Maximum-width counters everywhere.
	full := uint64(math.MaxUint64)
	for _, b := range c.interaction.decisionBuckets {
		for i := range b.outcomes {
			b.outcomes[i] = full
		}
		for i := range b.inferenceClasses {
			b.inferenceClasses[i] = full
		}
		for d := range b.scores {
			for i := range b.scores[d].counts {
				b.scores[d].counts[i] = full
			}
		}
		saturateHistogram(&b.decisionLatency)
		saturateHistogram(&b.inferenceLatency)
	}
	for _, b := range c.interaction.executionBuckets {
		b.counters = executionCountersState{
			dispatched: [actionCount]uint64{full, full}, reactions: full, texts: full, otherSideEffects: full,
			turnsWithEffect: full, turnsWithoutEffect: full, turnsFailed: full, turnsInterrupted: full,
			failures: [execFailureCount - 1]uint64{full, full, full, full, full},
		}
		saturateHistogram(&b.duration)
	}

	bound := interactionWorstCase()
	if got := len(c.interaction.decisionBuckets); got != bound.decisionBuckets {
		t.Fatalf("decision buckets = %d, derived worst case %d", got, bound.decisionBuckets)
	}
	if got := len(c.interaction.executionBuckets); got != bound.executionBuckets {
		t.Fatalf("execution buckets = %d, derived worst case %d", got, bound.executionBuckets)
	}

	_, encode, dirty := c.InteractionDataset().Capture()
	if !dirty {
		t.Fatal("expected a dirty dataset")
	}
	payload, err := encode()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("interaction worst case: %d decision + %d execution buckets, payload %.1f MB, derived bound %.1f MB, counters in RAM %.1f MB",
		bound.decisionBuckets, bound.executionBuckets, float64(len(payload))/(1<<20), float64(bound.payloadBytes)/(1<<20), float64(bound.counterBytesRAM)/(1<<20))
	if len(payload) > bound.payloadBytes {
		t.Fatalf("payload %d bytes exceeds the derived bound %d", len(payload), bound.payloadBytes)
	}
	if bound.payloadBytes >= MaxSnapshotPayloadBytes {
		t.Fatalf("derived bound %d does not fit the %d byte write limit", bound.payloadBytes, MaxSnapshotPayloadBytes)
	}
}
