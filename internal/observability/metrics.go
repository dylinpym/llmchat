// Package observability centralises metrics so the service layer never imports
// a metrics library directly.
package observability

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics covers the numbers that actually matter for an LLM chat backend:
// time-to-first-token (the latency users perceive), token spend (the cost), and
// how often history has to be dropped (the quality signal).
type Metrics struct {
	requests      *prometheus.CounterVec
	requestDur    *prometheus.HistogramVec
	generations   prometheus.Counter
	interrupted   prometheus.Counter
	timeToFirst   prometheus.Histogram
	genDuration   prometheus.Histogram
	tokens        *prometheus.CounterVec
	packDropped   *prometheus.CounterVec
	packSummarise *prometheus.HistogramVec
	packTokens    prometheus.Histogram
	saveFailures  prometheus.Counter
	activeStreams prometheus.Gauge
	dbConns       *prometheus.GaugeVec
}

func NewMetrics() *Metrics {
	factory := promauto.With(prometheus.DefaultRegisterer)
	return &Metrics{
		requests: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: "llmchat", Name: "http_requests_total",
			Help: "HTTP requests by route, method and status class.",
		}, []string{"route", "method", "status"}),

		requestDur: factory.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "llmchat", Name: "http_request_duration_seconds",
			Help:    "End-to-end HTTP latency. Buckets span ms to minutes because streaming responses are legitimately slow.",
			Buckets: []float64{.005, .01, .05, .1, .5, 1, 5, 15, 60, 180},
		}, []string{"route"}),

		generations: factory.NewCounter(prometheus.CounterOpts{
			Namespace: "llmchat", Name: "assistant_messages_total",
			Help: "Assistant messages generated.",
		}),
		interrupted: factory.NewCounter(prometheus.CounterOpts{
			Namespace: "llmchat", Name: "assistant_messages_interrupted_total",
			Help: "Generations stopped by client disconnect. A rising rate usually means timeouts are too aggressive.",
		}),
		timeToFirst: factory.NewHistogram(prometheus.HistogramOpts{
			Namespace: "llmchat", Name: "time_to_first_token_seconds",
			Help:    "Perceived responsiveness; the metric users actually feel.",
			Buckets: []float64{.05, .1, .25, .5, 1, 2, 5, 10, 30},
		}),
		genDuration: factory.NewHistogram(prometheus.HistogramOpts{
			Namespace: "llmchat", Name: "generation_duration_seconds",
			Buckets: []float64{.25, 1, 3, 10, 30, 60, 120, 300},
		}),
		tokens: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: "llmchat", Name: "tokens_total",
			Help: "Tokens by direction; the input to cost reporting.",
		}, []string{"direction", "model"}),

		packDropped: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: "llmchat", Name: "context_messages_dropped_total",
			Help: "History messages omitted from a request, by strategy. Non-zero means truncation is biting users.",
		}, []string{"strategy"}),
		packSummarise: factory.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "llmchat", Name: "context_messages_summarised",
			Help:    "How much history got folded into a summary per request.",
			Buckets: []float64{0, 1, 5, 10, 25, 50, 100, 250},
		}, []string{"strategy"}),
		packTokens: factory.NewHistogram(prometheus.HistogramOpts{
			Namespace: "llmchat", Name: "context_prompt_tokens",
			Help:    "Prompt size after packing; watch it approach the model window.",
			Buckets: []float64{100, 500, 1000, 4000, 8000, 16000, 32000, 64000, 128000},
		}),

		saveFailures: factory.NewCounter(prometheus.CounterOpts{
			Namespace: "llmchat", Name: "message_save_failures_total",
			Help: "Generated replies that could not be persisted — silent data loss, must stay at zero.",
		}),
		activeStreams: factory.NewGauge(prometheus.GaugeOpts{
			Namespace: "llmchat", Name: "active_streams",
			Help: "In-flight streaming generations.",
		}),
		dbConns: factory.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "llmchat", Name: "db_connections",
			Help: "Pool utilisation; saturation is the usual cause of latency cliffs.",
		}, []string{"state"}),
	}
}

func (m *Metrics) ObserveRequest(route, method string, status int, d time.Duration) {
	m.requests.WithLabelValues(route, method, statusClass(status)).Inc()
	m.requestDur.WithLabelValues(route).Observe(d.Seconds())
}

func (m *Metrics) ObservePack(strategy string, dropped, summarised, tokens int) {
	if dropped > 0 {
		m.packDropped.WithLabelValues(strategy).Add(float64(dropped))
	}
	m.packSummarise.WithLabelValues(strategy).Observe(float64(summarised))
	m.packTokens.Observe(float64(tokens))
}

func (m *Metrics) ObserveGeneration(total, firstTTFT time.Duration, promptTok, completionTok int, interrupted bool) {
	m.generations.Inc()
	m.genDuration.Observe(total.Seconds())
	if firstTTFT > 0 {
		m.timeToFirst.Observe(firstTTFT.Seconds())
	}
	if interrupted {
		m.interrupted.Inc()
	}
	_ = promptTok // direction labels are recorded by the caller with the model name
	_ = completionTok
}

func (m *Metrics) ObserveTokens(direction, model string, n int) {
	if n > 0 {
		m.tokens.WithLabelValues(direction, model).Add(float64(n))
	}
}

func (m *Metrics) IncSaveFailure() { m.saveFailures.Inc() }
func (m *Metrics) StreamStarted()  { m.activeStreams.Inc() }
func (m *Metrics) StreamFinished() { m.activeStreams.Dec() }

// SetPoolStats records connection pool occupancy. It takes plain ints rather
// than sql.DBStats because pgxpool exposes its own Stat type and does not
// guarantee the database/sql shape.
func (m *Metrics) SetPoolStats(inUse, idle, open, maxOpen int) {
	m.dbConns.WithLabelValues("in_use").Set(float64(inUse))
	m.dbConns.WithLabelValues("idle").Set(float64(idle))
	m.dbConns.WithLabelValues("open").Set(float64(open))
	m.dbConns.WithLabelValues("max_open").Set(float64(maxOpen))
}

func statusClass(status int) string {
	switch {
	case status >= 500:
		return "5xx"
	case status >= 400:
		return "4xx"
	default:
		return "2xx"
	}
}

// Handler serves /metrics. It is intentionally not registered on the public
// router — put it on the internal listener so it is not internet-exposed.
func Handler() http.Handler { return promhttp.Handler() }
