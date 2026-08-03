"""Generates opencode's provider config from a generic multi-provider
credential list, so adding a new API key is a one-line .env edit —
never a code change.

Convention (parallel comma-separated lists, same order/length):

  API_KEY=<key-1>,<key-2>,...
  API_URL=<base-url-1>,<base-url-2>,...

Each (key, url) pair becomes one opencode provider, named from the
URL's hostname (``openrouter.ai`` -> ``openrouter``). The dispatched
harness agent then references models as ``<provider-id>/<model-id>``.

Real incident this fixes: the harness profile's ``opencode/*`` model
prefix routes through opencode's own shared, unauthenticated free-tier
proxy — not the user's actual OpenRouter account — so its rate limit
is shared across every opencode user on the internet and has nothing
to do with the user's own usage. Every provider generated here uses a
real, user-supplied key against its real API, same as any other client.
"""
from __future__ import annotations

import json
import os
import re
import urllib.request
from pathlib import Path
from urllib.parse import urlparse

DEFAULT_CONFIG_PATH = Path.home() / ".config" / "opencode" / "opencode.jsonc"

# Known hosts get a curated set of genuinely free models (queried once,
# cached in the generated config) rather than the full paid+free catalog.
_KNOWN_FREE_MODELS: dict[str, list[str]] = {
    # z-ai/glm-5.2 exists on OpenRouter but has no :free variant (verified
    # via GET /v1/models — it's a paid-only model there) — don't list it
    # here even though it's genuinely free on NVIDIA NIM below.
    "openrouter.ai": [
        "nvidia/nemotron-3-ultra-550b-a55b:free",
        "openai/gpt-oss-20b:free",
    ],
    "integrate.api.nvidia.com": [
        "nvidia/nemotron-3-ultra-550b-a55b",
        "z-ai/glm-5.2",
        "moonshotai/kimi-k2.6",
    ],
}


def _provider_id_from_url(url: str) -> str:
    host = urlparse(url).hostname or url
    host = host.removeprefix("www.").removeprefix("api.").removeprefix("integrate.")
    slug = re.sub(r"[^a-z0-9]+", "-", host.lower()).strip("-")
    # integrate.api.nvidia.com -> nvidia.com -> nvidia (short, memorable)
    slug = slug.split("-")[0] if "nvidia" in slug else slug
    if "nvidia" in host:
        return "nvidia-nim"
    if "openrouter" in host:
        return "openrouter"
    return slug or "custom-provider"


def _npm_package_for(url: str) -> str:
    return "@openrouter/ai-sdk-provider" if "openrouter" in url else "@ai-sdk/openai-compatible"


def _free_models_for(url: str) -> dict[str, dict]:
    host = urlparse(url).hostname or ""
    for known_host, models in _KNOWN_FREE_MODELS.items():
        if known_host in host:
            return {m: {} for m in models}
    return {}


def parse_provider_list(api_keys: str, api_urls: str) -> list[dict]:
    """Parse the parallel API_KEY / API_URL env strings into a list of
    {id, url, env_var} dicts. Mismatched lengths are truncated to the
    shorter list — never raises, since a misconfigured extra entry on
    one side shouldn't break every other configured provider."""
    keys = [k.strip() for k in api_keys.split(",") if k.strip()]
    urls = [u.strip() for u in api_urls.split(",") if u.strip()]
    pairs = list(zip(keys, urls))
    out = []
    for key, url in pairs:
        provider_id = _provider_id_from_url(url)
        out.append({
            "id": provider_id,
            "url": url,
            "key": key,
            "env_var": f"{provider_id.upper().replace('-', '_')}_API_KEY",
        })
    return out


def generate_provider_config(api_keys: str, api_urls: str) -> dict:
    """Build the opencode.jsonc "provider" block content."""
    providers = parse_provider_list(api_keys, api_urls)
    config: dict = {}
    for p in providers:
        config[p["id"]] = {
            "npm": _npm_package_for(p["url"]),
            "env": [p["env_var"]],
            "options": {"baseURL": p["url"]},
            "models": _free_models_for(p["url"]),
        }
    return config


def write_opencode_config(
    api_keys: str, api_urls: str,
    config_path: Path = DEFAULT_CONFIG_PATH,
) -> list[dict]:
    """Write the generated provider block into opencode.jsonc, preserving
    any other top-level keys already in the file. Also returns the
    parsed provider list so the caller can export each provider's env
    var into this process's environment (so it reaches the opencode
    subprocess, which inherits AGW's environment)."""
    providers = parse_provider_list(api_keys, api_urls)
    existing: dict = {}
    if config_path.exists():
        try:
            # opencode.jsonc allows JS-style comments; strip full-line
            # `//` comments before parsing (only comment style we write).
            raw = config_path.read_text()
            raw = re.sub(r"^\s*//.*$", "", raw, flags=re.MULTILINE)
            existing = json.loads(raw)
        except (OSError, json.JSONDecodeError):
            existing = {}

    existing.setdefault("$schema", "https://opencode.ai/config.json")
    existing["provider"] = generate_provider_config(api_keys, api_urls)

    config_path.parent.mkdir(parents=True, exist_ok=True)
    config_path.write_text(json.dumps(existing, indent=2) + "\n")

    for p in providers:
        os.environ.setdefault(p["env_var"], p["key"])

    return providers


def free_model_ids(providers: list[dict]) -> list[str]:
    """Return every '<provider>/<model>' id available across configured
    providers, for use as a harness model allowlist / fallback chain."""
    out = []
    for p in providers:
        for model in _free_models_for(p["url"]):
            out.append(f"{p['id']}/{model}")
    return out


def check_provider_reachable(provider: dict, timeout: float = 5.0) -> bool:
    """Best-effort liveness probe: GET <base_url>/models with the
    provider's key. Used to pick the first *actually available*
    provider rather than always trying them in a fixed order and
    waiting for a real dispatch to fail."""
    req = urllib.request.Request(
        provider["url"].rstrip("/") + "/models",
        headers={"Authorization": f"Bearer {provider['key']}"},
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return 200 <= resp.status < 300
    except Exception:
        return False


__all__ = [
    "parse_provider_list", "generate_provider_config",
    "write_opencode_config", "free_model_ids", "check_provider_reachable",
]
