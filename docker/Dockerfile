# Build stage
FROM golang:1.22-alpine AS builder

WORKDIR /build

# Copy everything so local replace paths exist
COPY . .

# Download dependencies
RUN go mod download

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o retracker ./cmd/retracker

# Final stage
FROM alpine:latest

# Runtime deps: bash for helper script, curl for tracker downloads, ca certs for TLS
RUN apk --no-cache add ca-certificates bash curl

WORKDIR /app
RUN mkdir -p /app/scripts

# Copy the binary from builder
COPY --from=builder /build/retracker .

# Copy entrypoint and helper scripts from repo
COPY --from=builder /build/docker/docker-entrypoint.sh /app/docker-entrypoint.sh
COPY --from=builder /build/scripts/update-forwarders.sh /app/scripts/update-forwarders.sh
RUN chmod +x /app/docker-entrypoint.sh /app/scripts/update-forwarders.sh

EXPOSE 6969 6969/udp

ENTRYPOINT ["/app/docker-entrypoint.sh"]
