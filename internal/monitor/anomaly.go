// Package monitor implements the polling loop, anomaly detection,
// correlation engine, incident management, and HTTP API for Monitower.
package monitor

import (
	"math"
	"time"
)

// AnomalySignal is emitted when a metric value exceeds its baseline threshold.
type AnomalySignal struct {
	TS        int64   `json:"ts"`
	Service   string  `json:"service"`
	Queue     string  `json:"queue"`
	Metric    string  `json:"metric"`
	Value     float64 `json:"value"`
	Baseline  float64 `json:"baseline"`
	Deviation float64 `json:"deviation"`
	Direction string  `json:"direction"` // "high" | "low"
}

// baseline holds a sliding window of values for a single (service/queue, metric) pair.
type baseline struct {
	values []timestampedValue
	window time.Duration // keep values within this duration
}

type timestampedValue struct {
	ts    int64
	value float64
}

const minSamples = 10

// add appends a value and prunes values outside the window.
func (b *baseline) add(ts int64, value float64) {
	b.values = append(b.values, timestampedValue{ts: ts, value: value})
	cutoff := ts - b.window.Milliseconds()
	i := 0
	for i < len(b.values) && b.values[i].ts < cutoff {
		i++
	}
	b.values = b.values[i:]
}

// stats returns mean and population stddev. Valid reports false when fewer than minSamples exist.
func (b *baseline) stats() (mean, stddev float64, valid bool) {
	if len(b.values) < minSamples {
		return 0, 0, false
	}
	var sum float64
	for _, v := range b.values {
		sum += v.value
	}
	mean = sum / float64(len(b.values))
	var variance float64
	for _, v := range b.values {
		d := v.value - mean
		variance += d * d
	}
	variance /= float64(len(b.values))
	stddev = math.Sqrt(variance)
	return mean, stddev, true
}

// Baselines manages per-key sliding windows.
type Baselines struct {
	window time.Duration
	data   map[string]*baseline // key: "svc:metric" or "q:queue:metric"
}

// NewBaselines creates a Baselines with the given sliding window duration.
func NewBaselines(window time.Duration) *Baselines {
	return &Baselines{
		window: window,
		data:   make(map[string]*baseline),
	}
}

func (bs *Baselines) get(key string) *baseline {
	b, ok := bs.data[key]
	if !ok {
		b = &baseline{window: bs.window}
		bs.data[key] = b
	}
	return b
}

// AddServiceMetric records a service metric sample.
func (bs *Baselines) AddServiceMetric(ts int64, service, metric string, value float64) {
	bs.get("svc:"+service+":"+metric).add(ts, value)
}

// AddQueueMetric records a queue metric sample.
func (bs *Baselines) AddQueueMetric(ts int64, queue, metric string, value float64) {
	bs.get("q:"+queue+":"+metric).add(ts, value)
}

// ServiceMetricStats returns baseline stats for a service metric.
func (bs *Baselines) ServiceMetricStats(service, metric string) (mean, stddev float64, valid bool) {
	b, ok := bs.data["svc:"+service+":"+metric]
	if !ok {
		return 0, 0, false
	}
	return b.stats()
}

// QueueMetricStats returns baseline stats for a queue metric.
func (bs *Baselines) QueueMetricStats(queue, metric string) (mean, stddev float64, valid bool) {
	b, ok := bs.data["q:"+queue+":"+metric]
	if !ok {
		return 0, 0, false
	}
	return b.stats()
}

// ServiceRow is a single service_metrics row as read from the DB.
type ServiceRow struct {
	TS           int64
	Service      string
	Throughput   float64
	ErrorRate    float64
	P50Latency   float64
	P99Latency   float64
	ActiveFaults string
}

// QueueRow is a single queue_metrics row as read from the DB.
type QueueRow struct {
	TS          int64
	Queue       string
	Depth       float64
	EnqueueRate float64
	DequeueRate float64
	DLQ         bool
}

// DetectServiceAnomalies checks service metric rows against baselines and returns signals.
// It deduplicates signals per (service, metric) keeping the most extreme.
func DetectServiceAnomalies(rows []ServiceRow, bs *Baselines) []AnomalySignal {
	best := make(map[string]AnomalySignal) // key: "service:metric"

	for _, r := range rows {
		check := func(metric string, value float64) {
			mean, stddev, valid := bs.ServiceMetricStats(r.Service, metric)
			if !valid {
				return
			}
			var sig *AnomalySignal
			switch metric {
			case "error_rate":
				threshold := mean + 3*stddev
				if value > threshold && value >= 0.10 {
					dev := deviation(value, mean, stddev)
					sig = &AnomalySignal{TS: r.TS, Service: r.Service, Metric: metric,
						Value: value, Baseline: mean, Deviation: dev, Direction: "high"}
				}
			case "p99_latency":
				threshold := mean + 3*stddev
				if value > threshold && value >= 500 {
					dev := deviation(value, mean, stddev)
					sig = &AnomalySignal{TS: r.TS, Service: r.Service, Metric: metric,
						Value: value, Baseline: mean, Deviation: dev, Direction: "high"}
				}
			case "throughput":
				threshold := mean - 2*stddev
				if value < threshold && value < 0.5 {
					dev := deviation(value, mean, stddev)
					sig = &AnomalySignal{TS: r.TS, Service: r.Service, Metric: metric,
						Value: value, Baseline: mean, Deviation: dev, Direction: "low"}
				}
			}
			if sig == nil {
				return
			}
			key := r.Service + ":" + metric
			existing, ok := best[key]
			if !ok || math.Abs(sig.Deviation) > math.Abs(existing.Deviation) {
				best[key] = *sig
			}
		}
		check("error_rate", r.ErrorRate)
		check("p99_latency", r.P99Latency)
		check("throughput", r.Throughput)
	}

	out := make([]AnomalySignal, 0, len(best))
	for _, s := range best {
		out = append(out, s)
	}
	return out
}

// DetectQueueAnomalies checks queue metric rows against baselines and returns signals.
func DetectQueueAnomalies(rows []QueueRow, bs *Baselines) []AnomalySignal {
	best := make(map[string]AnomalySignal)

	for _, r := range rows {
		// DLQ: any depth > 0 is always an anomaly.
		if r.DLQ && r.Depth > 0 {
			key := r.Queue + ":dlq_depth"
			existing, ok := best[key]
			if !ok || r.Depth > existing.Value {
				best[key] = AnomalySignal{
					TS: r.TS, Queue: r.Queue, Metric: "dlq_depth",
					Value: r.Depth, Baseline: 0, Deviation: r.Depth, Direction: "high",
				}
			}
			continue
		}

		// Queue depth high.
		mean, stddev, valid := bs.QueueMetricStats(r.Queue, "depth")
		if valid {
			threshold := mean + 2*stddev
			if r.Depth > threshold && r.Depth >= 20 {
				key := r.Queue + ":queue_depth"
				dev := deviation(r.Depth, mean, stddev)
				existing, ok := best[key]
				if !ok || math.Abs(dev) > math.Abs(existing.Deviation) {
					best[key] = AnomalySignal{
						TS: r.TS, Queue: r.Queue, Metric: "queue_depth",
						Value: r.Depth, Baseline: mean, Deviation: dev, Direction: "high",
					}
				}
			}
		}
	}

	out := make([]AnomalySignal, 0, len(best))
	for _, s := range best {
		out = append(out, s)
	}
	return out
}

func deviation(value, mean, stddev float64) float64 {
	if stddev == 0 {
		return 0
	}
	return (value - mean) / stddev
}
