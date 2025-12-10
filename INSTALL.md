# Installation and Deployment

## Prerequisites
- Go 1.22+ for `go install` or building from source.
- Optional: Docker / Docker Compose for containerized runs.

## Install the binary
```bash
go install github.com/fl0v/retracker@latest
```
The binary is placed in `$GOBIN` (defaults to `$GOPATH/bin` or `$HOME/go/bin`).

## Run locally

### HTTP only
```bash
retracker -l :6969 -d
```
Announce URL: `http://<your ip>:6969/announce`

### HTTP + UDP
```bash
retracker -l :6969 -u :6969 -d
```
UDP is BEP 15 compliant with IPv4/IPv6, secure connection IDs, and shared announce logic with HTTP.

### With forwarders (HTTP/HTTPS/UDP)
```bash
retracker -l :6969 -d -f forwarders.yml
```
Minimal `forwarders.yml`:
```yaml
# HTTP trackers
- uri: https://tracker.example.com/announce

# UDP tracker
- uri: udp://tracker.example.com:6969/announce

# Optional extras
- uri: http://5.6.7.8:6969/announce
  ip: 192.168.1.15   # override IP when behind NAT
  host: retracker.local
  name: my-tracker
```
Auto-generate a list:
```bash
scripts/update-forwarders.sh
```

### Prometheus metrics
Enable in the config file with `prometheus_enabled: true`, then scrape `http://<ip>:<port>/metrics`.

### Reverse proxy (NGINX)
Example:
```nginx
server {
    listen 80;
    server_name retracker.local;
    proxy_set_header X-Real-IP $remote_addr;

    location /metrics {
        allow 10.0.0.0/8;
        deny all;
        proxy_pass http://localhost:6969;
    }

    location / {
        proxy_pass http://localhost:6969;
    }
}
```

## Docker

### Build and run
```bash
cd docker
docker build -t retracker .
docker run -d -p 6969:6969 retracker
```

### Docker Compose (recommended)
```bash
cd docker
docker-compose up -d
```
Key environment variables (map to CLI flags):
- `RETRACKER_CONFIG` → `-c` (path to config)
- `RETRACKER_LISTEN` → `-l` (HTTP listen)
- `RETRACKER_UDP_LISTEN` → `-u` (UDP listen, optional)
- `RETRACKER_FORWARDS` → `-f` (forwarders file)
- `RETRACKER_UPDATE_FORWARDERS` → when true, regenerate forwarders file on container start (writes to `RETRACKER_FORWARDS` path or default `configs/forwarders.yml`)
- `RETRACKER_DEBUG` → `-d` (enable debug)

## From source
```bash
git clone https://github.com/fl0v/retracker.git
cd retracker
go build ./cmd/retracker
./retracker -l :6969 -d
```

