"""Tests for metrics."""

from agents_gateway.metrics import MetricsRegistry, init_gateway_metrics, registry


class TestMetricsRegistry:
    def test_counter(self):
        r = MetricsRegistry()
        r.inc_counter("test_counter")
        assert r.get_counter("test_counter") == 1.0
        r.inc_counter("test_counter", 2)
        assert r.get_counter("test_counter") == 3.0

    def test_gauge(self):
        r = MetricsRegistry()
        r.set_gauge("test_gauge", 42)
        assert r.get_gauge("test_gauge") == 42

    def test_histogram(self):
        r = MetricsRegistry()
        r.observe_histogram("test_hist", 0.5)
        r.observe_histogram("test_hist", 1.5)
        assert len(r._histograms["test_hist"]) == 2

    def test_format_prometheus(self):
        r = MetricsRegistry()
        r.inc_counter("requests_total")
        r.set_gauge("agents_gateway_up", 1)
        output = r.format_prometheus()
        assert "requests_total" in output
        assert "agents_gateway_up" in output
        assert "# TYPE" in output

    def test_empty_registry(self):
        r = MetricsRegistry()
        assert r.format_prometheus() == ""


class TestInitMetrics:
    def test_init_gateway_metrics(self):
        r = MetricsRegistry()
        init_gateway_metrics(r)
        assert r.get_gauge("agents_gateway_up") == 1
