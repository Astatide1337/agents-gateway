"""Regression tests for the generic API_KEY/API_URL -> opencode provider
config generator (agents_gateway.harness.provider_config)."""
import json

from agents_gateway.harness.provider_config import (
    check_provider_reachable,
    free_model_ids,
    generate_provider_config,
    parse_provider_list,
    write_opencode_config,
)

OPENROUTER_URL = "https://openrouter.ai/api/v1"
NIM_URL = "https://integrate.api.nvidia.com/v1"


class TestParseProviderList:
    def test_parses_matching_pairs_in_order(self):
        providers = parse_provider_list("key-a,key-b", f"{OPENROUTER_URL},{NIM_URL}")
        assert [p["id"] for p in providers] == ["openrouter", "nvidia-nim"]
        assert providers[0]["key"] == "key-a"
        assert providers[1]["key"] == "key-b"

    def test_derives_conventional_env_var_names(self):
        providers = parse_provider_list("key-a,key-b", f"{OPENROUTER_URL},{NIM_URL}")
        assert providers[0]["env_var"] == "OPENROUTER_API_KEY"
        assert providers[1]["env_var"] == "NVIDIA_NIM_API_KEY"

    def test_mismatched_list_lengths_truncate_instead_of_raising(self):
        providers = parse_provider_list("only-one-key", f"{OPENROUTER_URL},{NIM_URL}")
        assert len(providers) == 1
        assert providers[0]["id"] == "openrouter"

    def test_blank_env_values_produce_no_providers(self):
        assert parse_provider_list("", "") == []

    def test_unknown_host_gets_a_slug_id_not_a_crash(self):
        providers = parse_provider_list("k", "https://api.example-llm.com/v1")
        assert providers[0]["id"] == "example-llm-com"


class TestGenerateProviderConfig:
    def test_known_providers_get_curated_free_models(self):
        config = generate_provider_config("key-a,key-b", f"{OPENROUTER_URL},{NIM_URL}")
        assert "nvidia/nemotron-3-ultra-550b-a55b:free" in config["openrouter"]["models"]
        assert "nvidia/nemotron-3-ultra-550b-a55b" in config["nvidia-nim"]["models"]

    def test_unknown_provider_gets_empty_model_dict_not_a_crash(self):
        config = generate_provider_config("k", "https://api.example-llm.com/v1")
        assert config["example-llm-com"]["models"] == {}

    def test_each_provider_declares_its_own_env_var_for_credential_lookup(self):
        config = generate_provider_config("key-a,key-b", f"{OPENROUTER_URL},{NIM_URL}")
        assert config["openrouter"]["env"] == ["OPENROUTER_API_KEY"]
        assert config["nvidia-nim"]["env"] == ["NVIDIA_NIM_API_KEY"]


class TestWriteOpencodeConfig:
    def test_writes_valid_json_with_schema_and_provider_block(self, tmp_path):
        path = tmp_path / "opencode.jsonc"
        write_opencode_config("key-a,key-b", f"{OPENROUTER_URL},{NIM_URL}", config_path=path)
        data = json.loads(path.read_text())
        assert data["$schema"] == "https://opencode.ai/config.json"
        assert set(data["provider"]) == {"openrouter", "nvidia-nim"}

    def test_preserves_unrelated_existing_top_level_keys(self, tmp_path):
        path = tmp_path / "opencode.jsonc"
        path.write_text(json.dumps({"$schema": "https://opencode.ai/config.json",
                                     "theme": "dark"}))
        write_opencode_config("key-a", OPENROUTER_URL, config_path=path)
        data = json.loads(path.read_text())
        assert data["theme"] == "dark"
        assert "openrouter" in data["provider"]

    def test_regenerating_drops_a_provider_removed_from_the_env_lists(self, tmp_path):
        path = tmp_path / "opencode.jsonc"
        write_opencode_config("key-a,key-b", f"{OPENROUTER_URL},{NIM_URL}", config_path=path)
        write_opencode_config("key-a", OPENROUTER_URL, config_path=path)
        data = json.loads(path.read_text())
        assert set(data["provider"]) == {"openrouter"}

    def test_exports_each_providers_key_into_process_env(self, tmp_path, monkeypatch):
        monkeypatch.delenv("OPENROUTER_API_KEY", raising=False)
        path = tmp_path / "opencode.jsonc"
        write_opencode_config("a-real-key", OPENROUTER_URL, config_path=path)
        import os
        assert os.environ["OPENROUTER_API_KEY"] == "a-real-key"

    def test_does_not_clobber_an_env_var_already_set_explicitly(self, tmp_path, monkeypatch):
        monkeypatch.setenv("OPENROUTER_API_KEY", "explicitly-configured-key")
        path = tmp_path / "opencode.jsonc"
        write_opencode_config("from-api-key-list", OPENROUTER_URL, config_path=path)
        import os
        assert os.environ["OPENROUTER_API_KEY"] == "explicitly-configured-key"


class TestFreeModelIds:
    def test_lists_provider_prefixed_model_ids_for_use_as_an_allowlist(self):
        providers = parse_provider_list("key-a,key-b", f"{OPENROUTER_URL},{NIM_URL}")
        ids = free_model_ids(providers)
        assert "openrouter/nvidia/nemotron-3-ultra-550b-a55b:free" in ids
        assert "nvidia-nim/nvidia/nemotron-3-ultra-550b-a55b" in ids

    def test_no_providers_yields_no_model_ids(self):
        assert free_model_ids([]) == []


class TestCheckProviderReachable:
    def test_unreachable_host_returns_false_not_a_raised_exception(self):
        provider = {"url": "https://this-host-does-not-exist.invalid/v1", "key": "k"}
        assert check_provider_reachable(provider, timeout=2.0) is False
