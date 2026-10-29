# syntax=docker/dockerfile:1

# Build stage: installs the package and its dependencies into a virtualenv,
# which is the only thing copied into the runtime image.
FROM python:3.14-slim AS build
ENV PIP_NO_CACHE_DIR=1 PIP_DISABLE_PIP_VERSION_CHECK=1
RUN python -m venv /venv
WORKDIR /src
# Copy only what the package build needs, so code edits elsewhere (tests,
# README) don't invalidate the dependency layer.
COPY pyproject.toml README.md LICENSE ./
COPY src ./src
RUN /venv/bin/pip install .

# Runtime stage: no compiler or build tools, runs as a non-root user.
FROM python:3.14-slim
ENV PATH=/venv/bin:$PATH PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1
COPY --from=build /venv /venv
COPY config.yaml config.real.yaml /etc/modelmux/
RUN useradd --system --no-create-home modelmux
USER modelmux
EXPOSE 8080
ENTRYPOINT ["python", "-m", "modelmux"]
CMD ["--config", "/etc/modelmux/config.yaml"]
