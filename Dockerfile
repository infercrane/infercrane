# syntax=docker/dockerfile:1.7
FROM python:3.12-slim AS builder

RUN apt-get update \
    && apt-get install -y --no-install-recommends build-essential cargo pkg-config \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /build
RUN python -m pip install --no-cache-dir uv==0.12.3
COPY pyproject.toml uv.lock README.md LICENSE ./
COPY src ./src
RUN uv sync --frozen --no-dev --extra router --no-editable

FROM python:3.12-slim AS runtime
RUN useradd --create-home --uid 10001 app
COPY --from=builder /build/.venv /opt/venv
COPY scripts/dev-start.sh /usr/local/bin/infercrane-dev-start
RUN chmod 755 /usr/local/bin/infercrane-dev-start \
    && mkdir -p /var/lib/infercrane \
    && chown app:app /var/lib/infercrane
ENV PATH="/opt/venv/bin:$PATH" \
    PYTHONUNBUFFERED=1 \
    INFERCRANE_STATE_DIR=/var/lib/infercrane \
    INFERCRANE_HOST=0.0.0.0
USER app
EXPOSE 8080
ENTRYPOINT ["infercrane"]
CMD ["serve"]
