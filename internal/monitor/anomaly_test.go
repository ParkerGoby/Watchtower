package monitor

import (
	"testing"
	"time"
)

func newTestBaselines() *Baselines {
	return NewBaselines(60 * time.Second)
}

// seedServiceBaseline adds n samples with the given value for service+metric.
func seedServiceBaseline(bs *Baselines, service, metric string, n int, value float64) {
	for i := 0; i < n; i++ {
		bs.AddServiceMetric(int64(i*2000), service, metric, value)
	}
}

// seedQueueBaseline adds n samples with the given value for queue+metric.
func seedQueueBaseline(bs *Baselines, queue, metric string, n int, value float64) {
	for i := 0; i < n; i++ {
		bs.AddQueueMetric(int64(i*2000), queue, metric, value)
	}
}

// --- Baseline readiness ---

func TestBaseline_NotReadyBelow10Samples(t *testing.T) {
	bs := newTestBaselines()
	for i := 0; i < 9; i++ {
		bs.AddServiceMetric(int64(i*2000), "payment", "error_rate", 0.01)
	}
	_, _, valid := bs.ServiceMetricStats("payment", "error_rate")
	if valid {
		t.Fatal("expected baseline to be invalid with < 10 samples")
	}
}

func TestBaseline_ReadyAt10Samples(t *testing.T) {
	bs := newTestBaselines()
	for i := 0; i < 10; i++ {
		bs.AddServiceMetric(int64(i*2000), "payment", "error_rate", 0.01)
	}
	_, _, valid := bs.ServiceMetricStats("payment", "error_rate")
	if !valid {
		t.Fatal("expected baseline to be valid with 10 samples")
	}
}

// --- Error rate detection ---

func TestDetect_ErrorRateSpike(t *testing.T) {
	bs := newTestBaselines()
	seedServiceBaseline(bs, "payment", "error_rate", 10, 0.02)
	// mean=0.02, stddev≈0 → threshold ≈ 0.02+3*0 = 0.02; use a clearly anomalous value
	// Since stddev is 0, to get above the threshold use a value well above and above 0.10 floor.
	rows := []ServiceRow{{TS: 30000, Service: "payment", ErrorRate: 0.50}}
	sigs := DetectServiceAnomalies(rows, bs)
	found := findSignal(sigs, "payment", "error_rate")
	if found == nil {
		t.Fatal("expected error_rate signal, got none")
	}
	if found.Direction != "high" {
		t.Errorf("expected direction high, got %s", found.Direction)
	}
}

func TestDetect_ErrorRateFloor_NoSignalBelow10Pct(t *testing.T) {
	bs := newTestBaselines()
	// All baseline values are 0; with stddev=0 the threshold is 0, but floor is 0.10.
	seedServiceBaseline(bs, "payment", "error_rate", 10, 0.00)
	// Value 0.05 is above threshold (0) but below floor (0.10) → no signal.
	rows := []ServiceRow{{TS: 30000, Service: "payment", ErrorRate: 0.05}}
	sigs := DetectServiceAnomalies(rows, bs)
	if findSignal(sigs, "payment", "error_rate") != nil {
		t.Fatal("expected no signal when error_rate below 0.10 floor")
	}
}

func TestDetect_ErrorRateAboveFloor(t *testing.T) {
	bs := newTestBaselines()
	seedServiceBaseline(bs, "payment", "error_rate", 10, 0.00)
	rows := []ServiceRow{{TS: 30000, Service: "payment", ErrorRate: 0.15}}
	sigs := DetectServiceAnomalies(rows, bs)
	if findSignal(sigs, "payment", "error_rate") == nil {
		t.Fatal("expected signal for error_rate above floor")
	}
}

// --- p99 latency detection ---

func TestDetect_P99LatencySpike(t *testing.T) {
	bs := newTestBaselines()
	seedServiceBaseline(bs, "payment", "p99_latency", 10, 100.0) // baseline 100ms
	rows := []ServiceRow{{TS: 30000, Service: "payment", P99Latency: 1200.0}}
	sigs := DetectServiceAnomalies(rows, bs)
	if findSignal(sigs, "payment", "p99_latency") == nil {
		t.Fatal("expected p99_latency signal")
	}
}

func TestDetect_P99LatencyFloor_NoSignalBelow500ms(t *testing.T) {
	bs := newTestBaselines()
	seedServiceBaseline(bs, "payment", "p99_latency", 10, 0.0)
	// Even though 0+3*0=0 < 400, value is below 500ms floor.
	rows := []ServiceRow{{TS: 30000, Service: "payment", P99Latency: 400.0}}
	sigs := DetectServiceAnomalies(rows, bs)
	if findSignal(sigs, "payment", "p99_latency") != nil {
		t.Fatal("expected no signal when p99_latency below 500ms floor")
	}
}

// --- Throughput drop detection ---

func TestDetect_ThroughputDrop(t *testing.T) {
	bs := newTestBaselines()
	seedServiceBaseline(bs, "fulfillment", "throughput", 10, 5.0)
	// mean=5, stddev=0 → threshold=5-2*0=5; any value below 5 AND below 0.5 triggers.
	rows := []ServiceRow{{TS: 30000, Service: "fulfillment", Throughput: 0.1}}
	sigs := DetectServiceAnomalies(rows, bs)
	sig := findSignal(sigs, "fulfillment", "throughput")
	if sig == nil {
		t.Fatal("expected throughput signal")
	}
	if sig.Direction != "low" {
		t.Errorf("expected direction low, got %s", sig.Direction)
	}
}

func TestDetect_ThroughputDrop_NoSignalIfNotBelowFloor(t *testing.T) {
	bs := newTestBaselines()
	seedServiceBaseline(bs, "fulfillment", "throughput", 10, 5.0)
	// Value is below mean-2σ but NOT below 0.5 floor.
	rows := []ServiceRow{{TS: 30000, Service: "fulfillment", Throughput: 4.0}}
	sigs := DetectServiceAnomalies(rows, bs)
	if findSignal(sigs, "fulfillment", "throughput") != nil {
		t.Fatal("expected no signal when throughput not below 0.5 floor")
	}
}

// --- Queue depth detection ---

func TestDetect_QueueDepthHigh(t *testing.T) {
	bs := newTestBaselines()
	seedQueueBaseline(bs, "order-queue", "depth", 10, 5.0)
	rows := []QueueRow{{TS: 30000, Queue: "order-queue", Depth: 50.0}}
	sigs := DetectQueueAnomalies(rows, bs)
	if findQueueSignal(sigs, "order-queue", "queue_depth") == nil {
		t.Fatal("expected queue_depth signal")
	}
}

func TestDetect_QueueDepthHigh_NoSignalBelow20Floor(t *testing.T) {
	bs := newTestBaselines()
	seedQueueBaseline(bs, "order-queue", "depth", 10, 0.0)
	// Value 15 is above threshold (0) but below floor (20).
	rows := []QueueRow{{TS: 30000, Queue: "order-queue", Depth: 15.0}}
	sigs := DetectQueueAnomalies(rows, bs)
	if findQueueSignal(sigs, "order-queue", "queue_depth") != nil {
		t.Fatal("expected no signal when depth below 20 floor")
	}
}

// --- DLQ detection ---

func TestDetect_DLQAnyDepthTriggersSignal(t *testing.T) {
	bs := newTestBaselines()
	// No baseline needed for DLQ.
	rows := []QueueRow{{TS: 30000, Queue: "payment-dlq", Depth: 1.0, DLQ: true}}
	sigs := DetectQueueAnomalies(rows, bs)
	if findQueueSignal(sigs, "payment-dlq", "dlq_depth") == nil {
		t.Fatal("expected dlq_depth signal")
	}
}

func TestDetect_DLQZeroDepth_NoSignal(t *testing.T) {
	bs := newTestBaselines()
	rows := []QueueRow{{TS: 30000, Queue: "payment-dlq", Depth: 0.0, DLQ: true}}
	sigs := DetectQueueAnomalies(rows, bs)
	if findQueueSignal(sigs, "payment-dlq", "dlq_depth") != nil {
		t.Fatal("expected no signal when DLQ depth is 0")
	}
}

// --- Deduplication ---

func TestDetect_DeduplicationKeepsMostExtreme(t *testing.T) {
	bs := newTestBaselines()
	seedServiceBaseline(bs, "payment", "error_rate", 10, 0.02)
	rows := []ServiceRow{
		{TS: 28000, Service: "payment", ErrorRate: 0.20}, // moderate spike
		{TS: 30000, Service: "payment", ErrorRate: 0.80}, // larger spike
	}
	sigs := DetectServiceAnomalies(rows, bs)
	count := 0
	var kept *AnomalySignal
	for i := range sigs {
		if sigs[i].Service == "payment" && sigs[i].Metric == "error_rate" {
			count++
			kept = &sigs[i]
		}
	}
	if count != 1 {
		t.Fatalf("expected 1 deduplicated signal, got %d", count)
	}
	if kept.Value != 0.80 {
		t.Errorf("expected most extreme value 0.80, got %f", kept.Value)
	}
}

// --- Helpers ---

func findSignal(sigs []AnomalySignal, service, metric string) *AnomalySignal {
	for i := range sigs {
		if sigs[i].Service == service && sigs[i].Metric == metric {
			return &sigs[i]
		}
	}
	return nil
}

func findQueueSignal(sigs []AnomalySignal, queue, metric string) *AnomalySignal {
	for i := range sigs {
		if sigs[i].Queue == queue && sigs[i].Metric == metric {
			return &sigs[i]
		}
	}
	return nil
}
