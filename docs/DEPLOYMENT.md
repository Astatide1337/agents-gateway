# Docker Deployment Reference

This document covers deploying Agent Gateway using Docker and Docker Compose.

## Dockerfile

The gateway ships a Dockerfile that builds a minimal production image. The image:

- Uses Python as the base runtime
- Installs dependencies via `uv`
- Copies the application source
- Exposes port 8902
- Defines a healthcheck against `/ready`
- Sets the default entrypoint to `agent-gateway`

Key build arguments:

| Argument | Description |
|---|---|
| `PORT` | HTTP server port (default: `8902`) |

## docker-compose.yml

A `docker-compose.yml` is provided for local deployment. It defines a single service:

```yaml
services:
  agent-gateway:
    build: .
    ports:
      - "8902:8902"
    volumes:
      - agent-gateway-data:/data
      - ./agents:/app/agents:ro
    environment:
      - AGW_AUTH__MODE=dev-none
      - AGW_OBSERVABILITY__LOG_LEVEL=info
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:8902/ready"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 10s
```

## .env.example

An `.env.example` file documents the available environment variables:

```env
AGW_AUTH__MODE=dev-none
AGW_SERVICE__PORT=8902
AGW_SERVICE__HOST=0.0.0.0
AGW_OBSERVABILITY__LOG_LEVEL=info
AGW_OBSERVABILITY__LOG_FORMAT=json
AGW_STORAGE__DIR=/data
AGW_STORAGE__SQLITE_URL=sqlite:////data/agent-gateway.db
```

Copy this file to `.env` and modify values before running `docker compose up`.

## Deploying Locally

### Using Docker Compose

```bash
# Copy environment file
cp .env.example .env

# Edit .env as needed

# Build and start
docker compose up --build

# Run in detached mode
docker compose up --build -d
```

### Using Docker Directly

```bash
# Build the image
docker build -t agent-gateway .

# Run the container
docker run -d \
  --name agent-gateway \
  -p 8902:8902 \
  -v agent-gateway-data:/data \
  -v $(pwd)/agents:/app/agents:ro \
  -e AGW_AUTH__MODE=dev-none \
  agent-gateway
```

## Volume Mounts

| Mount | Purpose | Access |
|---|---|---|
| `/data` | SQLite database and artifact storage | Read-write |
| `/app/agents` | Agent manifest directories | Read-only |

Mount the agents directory as read-only to prevent the gateway process from modifying agent manifests at runtime.

## Ports

| Port | Protocol | Description |
|---|---|---|
| `8902` | HTTP | Primary API endpoint, metrics, and healthchecks |

By default, the compose file maps `8902:8902`. To use a different host port, change the left side of the mapping:

```yaml
ports:
  - "9000:8902"
```

## Healthcheck

The Docker healthcheck queries the `/ready` endpoint. A healthy container returns HTTP 200 with:

```json
{
  "status": "ok"
}
```

The healthcheck runs every 30 seconds with a 5-second timeout and 3 retries. A start period of 10 seconds gives the gateway time to initialize before healthchecks begin counting failures.

You can check health manually:

```bash
curl -f http://localhost:8902/ready
```

## Stopping and Cleaning Up

```bash
# Stop containers
docker compose down

# Stop and remove volumes
docker compose down -v
```
