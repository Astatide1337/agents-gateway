"""Memory-pressure admission control for harness task dispatch.

Live incident: eight orphaned harness processes accumulated over one
night (see harness/reaper.py) and pushed host swap to 100% full, which
in turn made fork()/exec() fail or hang across the whole host — not
just for new harness dispatches, but for unrelated processes sharing
the machine. The worker had no way to know the host was in trouble; it
just kept claiming and spawning more.

This module reads ``/proc/meminfo`` directly rather than shelling out
to ``free`` — cheap (a few KB, no subprocess) and safe to call on every
poll iteration. Linux-only; ``is_memory_critical`` fails open (returns
False) on any parse error or non-Linux host, since refusing to ever
dispatch would be a worse failure mode than occasionally dispatching
under pressure it couldn't detect.
"""
from __future__ import annotations

__all__ = ["is_memory_critical", "read_meminfo"]


def read_meminfo(path: str = "/proc/meminfo") -> dict[str, int]:
    """Parse /proc/meminfo into {key: value_in_kb}. Empty dict on any
    read/parse failure (missing file, non-Linux host, malformed line)."""
    values: dict[str, int] = {}
    try:
        with open(path) as f:
            for line in f:
                parts = line.split(":", 1)
                if len(parts) != 2:
                    continue
                key = parts[0].strip()
                rest = parts[1].strip().split()
                if not rest:
                    continue
                try:
                    values[key] = int(rest[0])
                except ValueError:
                    continue
    except OSError:
        return {}
    return values


def is_memory_critical(
    min_available_mb: float = 512.0,
    min_swap_free_mb: float = 256.0,
    meminfo_path: str = "/proc/meminfo",
) -> bool:
    """True if the host is critically low on memory headroom.

    Two independent signals, either one being critical is enough: real
    available memory (``MemAvailable``, the kernel's own "how much can
    I hand out without swapping" estimate — more accurate than
    ``MemFree`` alone) below ``min_available_mb``, or free swap below
    ``min_swap_free_mb`` (a host with swap already nearly exhausted is
    one more heavy process away from the fork/exec failures seen in
    the incident this guards against, even if MemAvailable looks okay
    at this exact instant).
    """
    info = read_meminfo(meminfo_path)
    if not info:
        return False

    mem_available_kb = info.get("MemAvailable")
    if mem_available_kb is not None and mem_available_kb / 1024.0 < min_available_mb:
        return True

    swap_total_kb = info.get("SwapTotal")
    swap_free_kb = info.get("SwapFree")
    if swap_total_kb and swap_free_kb is not None and swap_total_kb > 0:
        if swap_free_kb / 1024.0 < min_swap_free_mb:
            return True

    return False
