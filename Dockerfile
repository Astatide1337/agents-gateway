# NOTE: not used by staging or production, and not built/published by any
# CI workflow. Production Agents Gateway containerization is explicitly
# deferred (see Astatide1337/infra/docs/ci-cd/architecture-decisions.md
# §3, §6) — the real deployment path is a pinned host-process release
# (infra/agents-gateway-release/). This file is kept only as a reference
# starting point for a future containerization decision and is known
# incomplete for that purpose today: it does not install `tmux` or `git`,
# both required by agents_gateway/harness/{tmux,git}.py for real
# (non-fake-tmux) harness sessions.
FROM python:3.12-slim

RUN pip install --no-cache-dir uv && \
    addgroup --system --gid 1000 appuser && \
    adduser --system --uid 1000 --gid 1000 appuser

WORKDIR /app

COPY pyproject.toml README.md ./
COPY agents_gateway/ agents_gateway/
COPY agents/ agents/

RUN uv pip install --system --no-cache .

RUN mkdir -p /data && chown -R appuser:appuser /app /data

USER appuser

EXPOSE 8092

CMD ["agents-gateway", "run"]
