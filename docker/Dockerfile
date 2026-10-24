# syntax=docker/dockerfile:1
# Build stage
FROM golang:1.27.3-alpine AS builder

WORKDIR /build

# Cache external dependencies independently of application source
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build the application
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o retracker ./cmd/retracker

# Final stage
FROM alpine:latest

# Runtime deps: bash for helper script, curl for tracker downloads, ca certs for TLS
RUN apk --no-cache add ca-certificates bash curl

WORKDIR /app
RUN mkdir -p /app/scripts

# Copy the binary from builder
COPY --from=builder /build/retracker .

# Copy entrypoint and helper scripts from repo
COPY --from=builder --chmod=0755 /build/docker/docker-entrypoint.sh /app/docker-entrypoint.sh
COPY --from=builder --chmod=0755 /build/scripts/update-forwarders.sh /app/scripts/update-forwarders.sh

EXPOSE 6969 6969/udp

ENTRYPOINT ["/app/docker-entrypoint.sh"]
