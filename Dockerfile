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
