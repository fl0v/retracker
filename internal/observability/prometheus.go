package observability

import (
	"log"
	"net/http"
	"os"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var ErrorLog = log.New(os.Stderr, `error#`, log.Lshortfile)

const forwarderNameLabel = "name"

type Prometheus struct {
	Requests                        prometheus.Counter
	ForwarderStatus                 *prometheus.CounterVec
	QueueDepth                      prometheus.Gauge
	QueueCapacity                   prometheus.Gauge
	QueueFillPct                    prometheus.Gauge
	DroppedFull                     prometheus.Counter
	RateLimited                     prometheus.Counter
	Throttled                       prometheus.Counter
	WorkerCount                     prometheus.Gauge
	DisabledForwarders              prometheus.Gauge
	SuspendedForwarders             prometheus.Gauge
	DisabledForwarderInfo           *prometheus.GaugeVec
	DisabledForwarderFailureCount   *prometheus.GaugeVec
	DisabledForwarderSecondsSince   *prometheus.GaugeVec
	SuspendedForwarderInfo          *prometheus.GaugeVec
	SuspendedForwarderSecondsRemain *prometheus.GaugeVec
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
		}, []string{forwarderNameLabel, `status`}),
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
		DisabledForwarders: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: `forwarders_disabled`,
			Help: `Current number of disabled (removed) forwarders`,
		}),
		SuspendedForwarders: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: `forwarders_suspended`,
			Help: `Current number of suspended forwarders`,
		}),
		DisabledForwarderInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: `forwarder_disabled`,
			Help: `Disabled forwarder marker with reason label (value is always 1 while disabled)`,
		}, []string{forwarderNameLabel, `reason`}),
		DisabledForwarderFailureCount: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: `forwarder_disabled_failure_count`,
			Help: `Failure count recorded when the forwarder was disabled`,
		}, []string{forwarderNameLabel}),
		DisabledForwarderSecondsSince: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: `forwarder_disabled_seconds_since`,
			Help: `Seconds elapsed since the forwarder was disabled`,
		}, []string{forwarderNameLabel}),
		SuspendedForwarderInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: `forwarder_suspended`,
			Help: `Suspended forwarder marker with reason label (value is always 1 while suspended)`,
		}, []string{forwarderNameLabel, `reason`}),
		SuspendedForwarderSecondsRemain: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: `forwarder_suspended_seconds_remaining`,
			Help: `Seconds remaining until suspension expires`,
		}, []string{forwarderNameLabel}),
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
	if err := prometheus.Register(p.DisabledForwarders); err != nil {
		ErrorLog.Println(err.Error())
		return nil, err
	}
	if err := prometheus.Register(p.SuspendedForwarders); err != nil {
		ErrorLog.Println(err.Error())
		return nil, err
	}
	if err := prometheus.Register(p.DisabledForwarderInfo); err != nil {
		ErrorLog.Println(err.Error())
		return nil, err
	}
	if err := prometheus.Register(p.DisabledForwarderFailureCount); err != nil {
		ErrorLog.Println(err.Error())
		return nil, err
	}
	if err := prometheus.Register(p.DisabledForwarderSecondsSince); err != nil {
		ErrorLog.Println(err.Error())
		return nil, err
	}
	if err := prometheus.Register(p.SuspendedForwarderInfo); err != nil {
		ErrorLog.Println(err.Error())
		return nil, err
	}
	if err := prometheus.Register(p.SuspendedForwarderSecondsRemain); err != nil {
		ErrorLog.Println(err.Error())
		return nil, err
	}
	http.Handle("/metrics", promhttp.Handler())
	return &p, nil
}
