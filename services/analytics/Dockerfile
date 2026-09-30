# Build context: repository root.
FROM python:3.12-slim AS build
ENV PIP_DISABLE_PIP_VERSION_CHECK=1 PIP_NO_CACHE_DIR=1
RUN --mount=type=secret,id=extra_ca,required=false \
    if [ -s /run/secrets/extra_ca ]; then cat /run/secrets/extra_ca >> /etc/ssl/certs/ca-certificates.crt; fi
ENV PIP_CERT=/etc/ssl/certs/ca-certificates.crt
WORKDIR /src
COPY services/analytics/pyproject.toml ./
RUN python -m venv /venv && /venv/bin/pip install --upgrade pip setuptools >/dev/null && \
    /venv/bin/pip install $(python -c "import tomllib;print(' '.join(tomllib.load(open('pyproject.toml','rb'))['project']['dependencies']))")

FROM python:3.12-slim
RUN useradd --uid 10001 --create-home app && mkdir -p /app/artifacts && chown 10001 /app/artifacts
COPY --from=build /venv /venv
COPY services/analytics/analytics /app/analytics
WORKDIR /app
ENV PATH=/venv/bin:$PATH PYTHONUNBUFFERED=1 ARTIFACTS_DIR=/app/artifacts
USER 10001
EXPOSE 9105
CMD ["python", "-m", "analytics.run", "schedule"]
