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
	}
	if err := prometheus.Register(p.Requests); err != nil {
		ErrorLog.Println(err.Error())
		return nil, err
	}
	if err := prometheus.Register(p.ForwarderStatus); err != nil {
		ErrorLog.Println(err.Error())
		return nil, err
	}
	http.Handle("/metrics", promhttp.Handler())
	return &p, nil
}
