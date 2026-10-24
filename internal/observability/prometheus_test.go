package observability

import (
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestPrometheusRegistriesAreIndependent(t *testing.T) {
	for i := range 2 {
		registry := prometheus.NewPedanticRegistry()
		metrics, err := NewPrometheus(registry)
		if err != nil {
			t.Fatal(err)
		}
		metrics.Requests.Add(float64(i + 1))
		metrics.ForwarderStatus.WithLabelValues("upstream", "ok").Inc()
		metrics.DisabledForwarderInfo.WithLabelValues("disabled", "failure").Set(1)
		metrics.DisabledForwarderFailureCount.WithLabelValues("disabled").Set(3)
		metrics.DisabledForwarderSecondsSince.WithLabelValues("disabled").Set(4)
		metrics.SuspendedForwarderInfo.WithLabelValues("suspended", "busy").Set(1)
		metrics.SuspendedForwarderSecondsRemain.WithLabelValues("suspended").Set(5)
		families, err := registry.Gather()
		if err != nil {
			t.Fatal(err)
		}
		if len(families) != 16 {
			t.Fatalf("got %d metric families, want 16", len(families))
		}
		for _, family := range families {
			if family.GetName() == "requests" && family.Metric[0].Counter.GetValue() != float64(i+1) {
				t.Fatal("request counter leaked between registries")
			}
		}
		if _, err := NewPrometheus(registry); err == nil {
			t.Fatal("duplicate registration accepted")
		}
	}
}

func TestPrometheusConflictDoesNotLeavePartialRegistration(t *testing.T) {
	registry := prometheus.NewRegistry()
	conflict := prometheus.NewGauge(prometheus.GaugeOpts{Name: "queue_depth", Help: "Current forwarder job queue depth"})
	registry.MustRegister(conflict)
	if _, err := NewPrometheus(registry); err == nil {
		t.Fatal("conflicting collector accepted")
	}
	if !registry.Unregister(conflict) {
		t.Fatal("could not remove conflict")
	}
	metrics, err := NewPrometheus(registry)
	if err != nil {
		t.Fatalf("failed registration left partial collectors: %v", err)
	}
	if !registry.Unregister(metrics) {
		t.Fatal("tracker metrics cannot be unregistered as a unit")
	}
	if _, err := NewPrometheus(registry); err != nil {
		t.Fatal(err)
	}
}

type failingRegisterer struct {
	prometheus.Registerer
	err error
}

func (r failingRegisterer) Register(prometheus.Collector) error { return r.err }

func TestPrometheusRegistrationErrorRetainsCause(t *testing.T) {
	want := errors.New("registration failed")
	metrics, err := NewPrometheus(failingRegisterer{err: want})
	if metrics != nil || !errors.Is(err, want) {
		t.Fatalf("got metrics=%v, error=%v", metrics, err)
	}
}
