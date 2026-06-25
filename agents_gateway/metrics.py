"""Prometheus-compatible metrics for Agents Gateway."""

from __future__ import annotations

import threading
from typing import Any


class MetricsRegistry:
    def __init__(self) -> None:
        self._counters: dict[str, float] = {}
        self._gauges: dict[str, float] = {}
        self._histograms: dict[str, list[float]] = {}
        self._lock = threading.Lock()

    def inc_counter(self, name: str, value: float = 1.0) -> None:
        with self._lock:
            self._counters[name] = self._counters.get(name, 0.0) + value

    def set_gauge(self, name: str, value: float) -> None:
        with self._lock:
            self._gauges[name] = value

    def observe_histogram(self, name: str, value: float) -> None:
        with self._lock:
            if name not in self._histograms:
                self._histograms[name] = []
            self._histograms[name].append(value)

    def get_counter(self, name: str) -> float:
        return self._counters.get(name, 0.0)

    def get_gauge(self, name: str) -> float:
        return self._gauges.get(name, 0.0)

    def format_prometheus(self) -> str:
        lines: list[str] = []
        with self._lock:
            for name, value in sorted(self._counters.items()):
                lines.append(f"# TYPE {name} counter")
                lines.append(f"{name} {value:.0f}")
            for name, value in sorted(self._gauges.items()):
                lines.append(f"# TYPE {name} gauge")
                lines.append(f"{name} {value:.0f}")
            for name, values in sorted(self._histograms.items()):
                lines.append(f"# TYPE {name} summary")
                lines.append(f'{name}_count {len(values)}')
                if values:
                    lines.append(f'{name}_sum {sum(values):.6f}')
        return "\n".join(lines) + "\n" if lines else ""


registry = MetricsRegistry()


def init_gateway_metrics(reg: MetricsRegistry | None = None) -> None:
    r = reg or registry
    r.set_gauge("agents_gateway_up", 1)
    r.set_gauge("agents_gateway_ready", 1)
    r.set_gauge("agents_total", 0)
    r.set_gauge("agents_invalid_total", 0)
    r.set_gauge("active_runs", 0)
    for name in (
        "tasks_total", "tasks_created_total", "tasks_completed_total",
        "tasks_failed_total", "tasks_cancelled_total", "artifacts_total",
        "requests_total", "request_errors_total",
    ):
        r.inc_counter(name, 0)
