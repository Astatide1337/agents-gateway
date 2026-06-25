"""Tests for CLI commands."""

from typer.testing import CliRunner

from agents_gateway.cli import app

runner = CliRunner()


class TestVersion:
    def test_version(self):
        result = runner.invoke(app, ["version"])
        assert result.exit_code == 0
        assert "0.1.0" in result.stdout


class TestList:
    def test_list_with_agents_dir(self, tmp_path, monkeypatch):
        agents_dir = tmp_path / "agents"
        agents_dir.mkdir()
        agent_dir = agents_dir / "test-agent"
        agent_dir.mkdir()
        (agent_dir / "agent.yaml").write_text(
            "id: test-agent\nname: Test Agent\ndescription: A test agent\nversion: 0.1.0\nruntime:\n  type: local-stub\n"
        )
        monkeypatch.chdir(tmp_path)
        result = runner.invoke(app, ["list", "--config", str(tmp_path / "nonexistent.yaml")])
        assert result.exit_code == 0

    def test_list_no_agents(self, tmp_path, monkeypatch):
        agents_dir = tmp_path / "agents"
        agents_dir.mkdir()
        monkeypatch.chdir(tmp_path)
        result = runner.invoke(app, ["list", "--config", str(tmp_path / "nonexistent.yaml")])
        assert "No agents found" in result.stdout


class TestValidate:
    def test_validate_no_agents(self, tmp_path, monkeypatch):
        agents_dir = tmp_path / "agents"
        agents_dir.mkdir()
        monkeypatch.chdir(tmp_path)
        result = runner.invoke(app, ["validate", "--config", str(tmp_path / "nonexistent.yaml")])
        assert result.exit_code == 0


class TestDoctor:
    def test_doctor(self, tmp_path, monkeypatch):
        agents_dir = tmp_path / "agents"
        agents_dir.mkdir()
        monkeypatch.chdir(tmp_path)
        result = runner.invoke(app, ["doctor", "--config", str(tmp_path / "nonexistent.yaml")])
        assert "agents-gateway" in result.stdout or "Auth mode" in result.stdout

    def test_doctor_missing_agents_dir(self, tmp_path, monkeypatch):
        monkeypatch.chdir(tmp_path)
        result = runner.invoke(app, ["doctor", "--config", str(tmp_path / "nonexistent.yaml")])
        assert result.exit_code == 1


class TestInspect:
    def test_inspect_missing(self, tmp_path, monkeypatch):
        monkeypatch.chdir(tmp_path)
        result = runner.invoke(app, ["inspect", "nonexistent", "--config", str(tmp_path / "nonexistent.yaml")])
        assert result.exit_code == 1
