// examples for testing

package testdata

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"k8s.io/component-base/metrics"
)

func main() {
	ch := make(chan<- prometheus.Metric)

	// counter metric should have _total suffix
	_ = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "test_metric_name",
			Help: "test help text",
		},
		[]string{},
	)

	// no help text
	_ = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "test_metric_total",
		},
		[]string{},
	)

	// NewCounterFunc, should have _total suffix
	_ = promauto.NewCounterFunc(prometheus.CounterOpts{
		Name: "foo",
		Help: "bar",
	}, func() float64 {
		return 1
	})

	// good
	f := promauto.With(prometheus.NewRegistry())
	_ = f.NewCounterVec(
		prometheus.CounterOpts{
			Name: "test_metric_total",
			Help: "",
		},
		[]string{},
	)

	// good
	_ = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "test_metric_total",
			Help: "",
		},
		[]string{},
	)

	// good
	desc := prometheus.NewDesc(
		"prometheus_operator_spec_replicas",
		"Number of expected replicas for the object.",
		[]string{
			"namespace",
			"name",
		}, nil,
	)
	ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, 1)

	// support using BuildFQName to generate fqName here.
	// bad metric, gauge shouldn't have _total
	ch <- prometheus.MustNewConstMetric(prometheus.NewDesc(
		prometheus.BuildFQName("foo", "bar", "total"),
		"Number of expected replicas for the object.",
		[]string{
			"namespace",
			"name",
		}, nil), prometheus.GaugeValue, 1)

	// support detecting kubernetes metrics
	kubeMetricDesc := metrics.NewDesc(
		"kube_test_metric_count",
		"Gauge Help",
		[]string{}, nil, metrics.STABLE, "",
	)
	ch <- metrics.NewLazyConstMetric(kubeMetricDesc, metrics.GaugeValue, 1)

	// bad
	_ = metrics.NewHistogram(&metrics.HistogramOpts{
		Name: "test_histogram_duration_seconds",
		Help: "",
	})
}
