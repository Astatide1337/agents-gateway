# Agents Gateway

Production-grade agent gateway with CLI, MCP tools, task lifecycle, and observability.

## Quick Start

```bash
uv sync
agents-gateway run
```

## CLI Commands

```bash
agents-gateway run              # Start the gateway server
agents-gateway validate         # Validate agent manifests and config
agents-gateway list             # List available agents
agents-gateway inspect <id>     # Inspect a specific agent
agents-gateway doctor           # Diagnose gateway health
agents-gateway version          # Print version
```

## Docker

```bash
cp .env.example .env
docker compose up -d --build
```

## Architecture

See [docs/AGENTS_GATEWAY_STANDARD.md](docs/AGENTS_GATEWAY_STANDARD.md) for the full specification.
