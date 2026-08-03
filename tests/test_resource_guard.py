"""Tests for the memory-pressure admission-control guard (see
resource_guard.py's docstring for the live incident this closes)."""
from __future__ import annotations

from agents_gateway.resource_guard import is_memory_critical, read_meminfo


def _write_meminfo(tmp_path, mem_available_kb=None, swap_total_kb=None, swap_free_kb=None,
                    extra_lines=None):
    lines = []
    lines.append("MemTotal:       10000000 kB")
    lines.append("MemFree:         2000000 kB")
    if mem_available_kb is not None:
        lines.append(f"MemAvailable:   {mem_available_kb} kB")
    if swap_total_kb is not None:
        lines.append(f"SwapTotal:      {swap_total_kb} kB")
    if swap_free_kb is not None:
        lines.append(f"SwapFree:       {swap_free_kb} kB")
    for extra in (extra_lines or []):
        lines.append(extra)
    path = tmp_path / "meminfo"
    path.write_text("\n".join(lines) + "\n")
    return str(path)


class TestReadMeminfo:
    def test_parses_known_keys(self, tmp_path):
        path = _write_meminfo(tmp_path, mem_available_kb=1048576, swap_total_kb=4194304,
                               swap_free_kb=13312)
        info = read_meminfo(path)
        assert info["MemAvailable"] == 1048576
        assert info["SwapTotal"] == 4194304
        assert info["SwapFree"] == 13312

    def test_missing_file_returns_empty_dict_not_a_crash(self, tmp_path):
        assert read_meminfo(str(tmp_path / "does-not-exist")) == {}

    def test_malformed_line_is_skipped_not_fatal(self, tmp_path):
        path = _write_meminfo(
            tmp_path, mem_available_kb=1048576,
            extra_lines=["GarbageLineWithNoColon", "AnotherKey: not-a-number kB"],
        )
        info = read_meminfo(path)
        assert info["MemAvailable"] == 1048576
        assert "AnotherKey" not in info


class TestIsMemoryCritical:
    def test_healthy_memory_and_swap_is_not_critical(self, tmp_path):
        path = _write_meminfo(tmp_path, mem_available_kb=4 * 1024 * 1024,
                               swap_total_kb=4 * 1024 * 1024, swap_free_kb=3 * 1024 * 1024)
        assert is_memory_critical(meminfo_path=path) is False

    def test_low_mem_available_is_critical(self, tmp_path):
        path = _write_meminfo(tmp_path, mem_available_kb=100 * 1024,
                               swap_total_kb=4 * 1024 * 1024, swap_free_kb=3 * 1024 * 1024)
        assert is_memory_critical(min_available_mb=512, meminfo_path=path) is True

    def test_low_swap_free_is_critical_even_with_healthy_mem_available(self, tmp_path):
        """The exact live incident: MemAvailable can look fine at a given
        instant while swap is nearly exhausted — either signal alone
        must trip the guard."""
        path = _write_meminfo(tmp_path, mem_available_kb=4 * 1024 * 1024,
                               swap_total_kb=4 * 1024 * 1024, swap_free_kb=13312)
        assert is_memory_critical(min_swap_free_mb=256, meminfo_path=path) is True

    def test_no_swap_configured_only_checks_mem_available(self, tmp_path):
        path = _write_meminfo(tmp_path, mem_available_kb=4 * 1024 * 1024,
                               swap_total_kb=0, swap_free_kb=0)
        assert is_memory_critical(meminfo_path=path) is False

    def test_missing_meminfo_fails_open(self, tmp_path):
        assert is_memory_critical(meminfo_path=str(tmp_path / "nope")) is False

    def test_thresholds_are_configurable(self, tmp_path):
        path = _write_meminfo(tmp_path, mem_available_kb=600 * 1024,
                               swap_total_kb=4 * 1024 * 1024, swap_free_kb=3 * 1024 * 1024)
        assert is_memory_critical(min_available_mb=512, meminfo_path=path) is False
        assert is_memory_critical(min_available_mb=1024, meminfo_path=path) is True
