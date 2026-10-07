# Build context: a platform wheel and requirements.txt exported from uv.lock.
# Publish the result and use its OCI digest in container(...).
FROM python:3.12-slim@sha256:78387bc3881b8273120a12ebe6c1ab22b018ccc2c9adf565ae1ac9b536e184ea
COPY requirements.txt /tmp/requirements.txt
RUN pip install --no-cache-dir --require-hashes -r /tmp/requirements.txt \
    && rm /tmp/requirements.txt
COPY *.whl /tmp/wheels/
RUN pip install --no-cache-dir --no-deps /tmp/wheels/*.whl && rm -rf /tmp/wheels
# Each Python attempt checks the image against the workflow's archived
# pyproject.toml; a workflow with a uv.lock needs uv and a writable cache.
COPY --from=ghcr.io/astral-sh/uv:0.12.1@sha256:cf4eedcaa81655197f625739489effcbe71b61ceb1506f332c3facae5deceded /uv /usr/local/bin/uv
ENV UV_CACHE_DIR=/tmp/uv-cache
USER 65532:65532
WORKDIR /tmp
