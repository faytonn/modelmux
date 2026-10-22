# syntax=docker/dockerfile:1

# Build stage: full Go toolchain, discarded after the build.
FROM golang:1.27-alpine AS build
WORKDIR /src
# Download modules first so this layer is cached until go.mod/go.sum change.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO off gives a static binary that runs on a minimal base image.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/modelmux ./cmd/modelmux

# Runtime stage: no shell, no package manager, CA certificates included
# (needed for HTTPS to the providers), runs as a non-root user.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/modelmux /modelmux
COPY config.yaml config.real.yaml /etc/modelmux/
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/modelmux"]
CMD ["-config", "/etc/modelmux/config.yaml"]
