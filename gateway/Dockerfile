FROM python:3.12-slim

RUN pip install --no-cache-dir uv && \
    addgroup --system --gid 1000 appuser && \
    adduser --system --uid 1000 --gid 1000 appuser

WORKDIR /app

COPY server.py /app/server.py
COPY agents.yaml /app/agents.yaml

RUN uv pip install --system --no-cache \
    "fastapi>=0.115,<1" \
    "uvicorn[standard]>=0.32,<1" \
    "httpx>=0.27,<1" \
    "pyyaml>=6.0,<7" \
    "pyjwt>=2.8,<3" \
    "cryptography>=41.0,<43" \
    "python-multipart>=0.0.9,<1"

USER appuser

CMD ["uvicorn", "server:app", "--host", "0.0.0.0", "--port", "8092", "--log-level", "info"]
