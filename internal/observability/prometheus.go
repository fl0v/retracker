package observability

import (
	"log"
	"net/http"
	"os"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var ErrorLog = log.New(os.Stderr, `error#`, log.Lshortfile)

type Prometheus struct {
	Requests        prometheus.Counter
	ForwarderStatus *prometheus.CounterVec
	QueueDepth      prometheus.Gauge
	QueueCapacity   prometheus.Gauge
	QueueFillPct    prometheus.Gauge
	DroppedFull     prometheus.Counter
	RateLimited     prometheus.Counter
	Throttled       prometheus.Counter
	WorkerCount     prometheus.Gauge
}

func NewPrometheus() (*Prometheus, error) {
	p := Prometheus{
		Requests: prometheus.NewCounter(prometheus.CounterOpts{
			Name: `requests`,
			Help: `Announce requests`,
		}),
		ForwarderStatus: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: `forwarder_status`,
			Help: `Forwarder response status`,
		}, []string{`name`, `status`}),
		QueueDepth: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: `queue_depth`,
			Help: `Current forwarder job queue depth`,
		}),
		QueueCapacity: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: `queue_capacity`,
			Help: `Forwarder job queue capacity`,
		}),
		QueueFillPct: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: `queue_fill_pct`,
			Help: `Forwarder job queue utilization percent`,
		}),
		DroppedFull: prometheus.NewCounter(prometheus.CounterOpts{
			Name: `queue_dropped_full`,
			Help: `Jobs dropped because queue is full`,
		}),
		RateLimited: prometheus.NewCounter(prometheus.CounterOpts{
			Name: `queue_rate_limited`,
			Help: `Initial announces rejected due to rate limiting`,
		}),
		Throttled: prometheus.NewCounter(prometheus.CounterOpts{
			Name: `queue_throttled_forwarders`,
			Help: `Forwarders skipped due to throttling`,
		}),
		WorkerCount: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: `forwarder_workers`,
			Help: `Active forwarder workers`,
		}),
	}
	if err := prometheus.Register(p.Requests); err != nil {
		ErrorLog.Println(err.Error())
		return nil, err
	}
	if err := prometheus.Register(p.ForwarderStatus); err != nil {
		ErrorLog.Println(err.Error())
		return nil, err
	}
	if err := prometheus.Register(p.QueueDepth); err != nil {
		ErrorLog.Println(err.Error())
		return nil, err
	}
	if err := prometheus.Register(p.QueueCapacity); err != nil {
		ErrorLog.Println(err.Error())
		return nil, err
	}
	if err := prometheus.Register(p.QueueFillPct); err != nil {
		ErrorLog.Println(err.Error())
		return nil, err
	}
	if err := prometheus.Register(p.DroppedFull); err != nil {
		ErrorLog.Println(err.Error())
		return nil, err
	}
	if err := prometheus.Register(p.RateLimited); err != nil {
		ErrorLog.Println(err.Error())
		return nil, err
	}
	if err := prometheus.Register(p.Throttled); err != nil {
		ErrorLog.Println(err.Error())
		return nil, err
	}
	if err := prometheus.Register(p.WorkerCount); err != nil {
		ErrorLog.Println(err.Error())
		return nil, err
	}
	http.Handle("/metrics", promhttp.Handler())
	return &p, nil
}
