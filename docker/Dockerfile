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

RUN apk --no-cache add ca-certificates wget

WORKDIR /app

# Copy the binary from builder
COPY --from=builder /build/retracker .

# Copy entrypoint script from repo
COPY --from=builder /build/docker/docker-entrypoint.sh /app/docker-entrypoint.sh
RUN chmod +x /app/docker-entrypoint.sh

EXPOSE 6969 6969/udp

ENTRYPOINT ["/app/docker-entrypoint.sh"]
