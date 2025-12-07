# Build stage
FROM golang:1.22-alpine AS builder

# Install git for cloning
RUN apk add --no-cache git

WORKDIR /build

# Clone the repository
ARG REPO_URL=https://github.com/fl0v/retracker.git
ARG BRANCH=master
RUN git clone --depth 1 --branch ${BRANCH} ${REPO_URL} retracker

WORKDIR /build/retracker

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

# Copy entrypoint script from builder stage
COPY --from=builder /build/retracker/docker/docker-entrypoint.sh /app/docker-entrypoint.sh
RUN chmod +x /app/docker-entrypoint.sh

# Expose default ports (HTTP and UDP)
EXPOSE 6969 6969/udp

# Use entrypoint script
ENTRYPOINT ["/app/docker-entrypoint.sh"]
