"""CLI commands for agents-gateway."""

from __future__ import annotations

import typer

from agents_gateway import __version__

app = typer.Typer(name="agents-gateway", help="Agents Gateway CLI")


@app.command()
def run(
    config: str | None = typer.Option(None, "--config", "-c", help="Path to config YAML"),
    profile: str | None = typer.Option(None, "--profile", "-p", help="Active profile"),
    host: str | None = typer.Option(None, "--host", help="Bind host"),
    port: int | None = typer.Option(None, "--port", help="Bind port"),
    production: bool = typer.Option(False, "--production", help="Enforce production-safe auth mode"),
) -> None:
    """Start the gateway server."""
    from agents_gateway.auth import AuthHandler
    from agents_gateway.config import AuthConfig, load_config
    from agents_gateway.server import start_server

    cfg = load_config(config)
    if profile:
        cfg.profile = profile
    if host:
        cfg.service.host = host
    if port:
        cfg.service.port = port
    if production:
        handler = AuthHandler(cfg.auth)
        handler.require_production_safe()
    start_server(cfg)


@app.command()
def validate(
    config: str | None = typer.Option(None, "--config", "-c", help="Path to config YAML"),
) -> None:
    """Validate all agent manifests and configuration."""
    from agents_gateway.config import load_config
    from agents_gateway.catalog import AgentCatalog

    cfg = load_config(config)
    catalog = AgentCatalog(cfg)
    results = catalog.validate_all()
    errors = [r for r in results if r.severity == "error"]
    warnings = [r for r in results if r.severity == "warning"]

    if errors:
        for e in errors:
            typer.echo(f"ERROR [{e.agent_id}]: {e.message}", err=True)
    if warnings:
        for w in warnings:
            typer.echo(f"WARN  [{w.agent_id}]: {w.message}", err=True)

    if not errors and not warnings:
        typer.echo("All agents valid.")
    elif not errors:
        typer.echo(f"All agents valid ({len(warnings)} warnings).")
    else:
        raise typer.Exit(code=1)


@app.command(name="list")
def list_agents(
    config: str | None = typer.Option(None, "--config", "-c", help="Path to config YAML"),
    profile: str | None = typer.Option(None, "--profile", "-p", help="Active profile"),
) -> None:
    """List available agents."""
    from agents_gateway.config import load_config
    from agents_gateway.catalog import AgentCatalog

    cfg = load_config(config)
    if profile:
        cfg.profile = profile
    catalog = AgentCatalog(cfg)
    agents = catalog.list_agents()
    if not agents:
        typer.echo("No agents found.")
        return
    for a in agents:
        typer.echo(f"  {a.id:30s} {a.name:30s} {a.version:10s} {a.runtime.type}")


@app.command()
def inspect(
    agent_id: str = typer.Argument(..., help="Agent ID to inspect"),
    config: str | None = typer.Option(None, "--config", "-c", help="Path to config YAML"),
    profile: str | None = typer.Option(None, "--profile", "-p", help="Active profile"),
) -> None:
    """Show details for a specific agent."""
    import json

    from agents_gateway.config import load_config
    from agents_gateway.catalog import AgentCatalog

    cfg = load_config(config)
    if profile:
        cfg.profile = profile
    catalog = AgentCatalog(cfg)
    agent = catalog.get_agent(agent_id)
    if agent is None:
        typer.echo(f"Agent '{agent_id}' not found.", err=True)
        raise typer.Exit(code=1)
    typer.echo(json.dumps(agent.model_dump(), indent=2, default=str))


@app.command()
def doctor(
    config: str | None = typer.Option(None, "--config", "-c", help="Path to config YAML"),
) -> None:
    """Diagnose gateway health and configuration."""
    import os
    from pathlib import Path

    from agents_gateway.config import load_config

    cfg = load_config(config)
    ok = True

    typer.echo(f"Config file:      {'agents-gateway.yaml' if Path('agents-gateway.yaml').exists() else 'defaults'}")
    typer.echo(f"Service host:      {cfg.service.host}:{cfg.service.port}")
    typer.echo(f"Auth mode:         {cfg.auth.mode}")
    typer.echo(f"Agents dir:        {cfg.agents.dir}")
    typer.echo(f"Storage path:      {cfg.storage.sqlite_path}")
    typer.echo(f"Artifacts dir:     {cfg.storage.artifacts_dir}")
    typer.echo(f"Log level:         {cfg.observability.log_level}")
    typer.echo(f"Log format:        {cfg.observability.log_format}")
    typer.echo(f"Metrics enabled:   {cfg.observability.metrics_enabled}")
    typer.echo(f"Profile:           {cfg.profile or 'all'}")

    agents_dir = Path(cfg.agents.dir)
    if not agents_dir.exists():
        typer.echo(f"PROBLEM: Agents directory does not exist: {cfg.agents.dir}", err=True)
        ok = False
    else:
        count = sum(1 for d in agents_dir.iterdir() if d.is_dir() and (d / "agent.yaml").exists())
        typer.echo(f"Agent manifests:   {count}")

    if cfg.auth.mode == "dev-none":
        typer.echo("WARNING: Auth mode is dev-none (no authentication)", err=True)

    if ok:
        typer.echo("Doctor check: OK")
    else:
        raise typer.Exit(code=1)


@app.command()
def version() -> None:
    """Print version."""
    typer.echo(f"agents-gateway {__version__}")
