# =============================================================================
# Stage 0: Go builder
# =============================================================================
FROM golang:1.25-alpine AS go-builder

WORKDIR /app

COPY go.mod go.sum ./
COPY vendor/ ./vendor/

COPY *.go ./
COPY ffmpeg/ ./ffmpeg/
COPY media/ ./media/
COPY protocol/ ./protocol/
COPY subtitle/ ./subtitle/

RUN go build -mod=vendor -trimpath -o /streamline .

# =============================================================================
# Stage 1: Runtime
# =============================================================================
FROM debian:bookworm-slim

# Install ffmpeg, fonts, and CA certificates.
# Debian's ca-certificates package works reliably on Docker Desktop macOS.
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        ca-certificates \
        ffmpeg \
        fonts-liberation2 \
        fonts-dejavu-core && \
    groupadd --system streamline && \
    useradd --system --gid streamline --home-dir /nonexistent --no-create-home --shell /usr/sbin/nologin streamline && \
    rm -rf /var/lib/apt/lists/*

# Copy overlay assets into the image.
COPY assets/ /app/assets/

COPY --from=go-builder /streamline /usr/local/bin/streamline
COPY --chmod=755 entrypoint.sh /usr/local/bin/streamline-entrypoint

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/streamline-entrypoint"]
CMD ["/usr/local/bin/streamline"]
