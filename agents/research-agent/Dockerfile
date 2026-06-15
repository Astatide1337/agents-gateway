FROM python:3.12-slim

RUN pip install --no-cache-dir uv && \
    addgroup --system --gid 1000 appuser && \
    adduser --system --uid 1000 --gid 1000 appuser

WORKDIR /app

COPY .env /app/.env
COPY server.py /app/server.py

RUN uv pip install --system --no-cache \
    "fastapi>=0.115,<1" \
    "uvicorn[standard]>=0.32,<1" \
    "httpx>=0.27,<1" \
    "pydantic>=2.0,<3" \
    "aiohttp>=3.9,<4" \
    "aiosqlite>=0.20,<1"

RUN mkdir -p /data && chown -R appuser:appuser /app /data

USER appuser

CMD ["uvicorn", "server:app", "--host", "0.0.0.0", "--port", "8093", "--log-level", "info"]
