# Installation and Deployment

## Prerequisites
- Go 1.26+ (Go 1.27.3 recommended) for `go install` or building from source.
- Optional: Docker / Docker Compose for containerized runs.

## Install the binary
```bash
go install github.com/fl0v/retracker/cmd/retracker@latest
```
The binary is placed in `$GOBIN` (defaults to `$GOPATH/bin` or `$HOME/go/bin`).

## Run locally

### HTTP + UDP (recommended)
```bash
retracker -l :6969 -u :6969 -d
```
UDP is BEP 15 compliant with IPv4/IPv6, secure connection IDs, and shared announce logic with HTTP.
Prefer `udp://<your ip>:6969/announce` in your torrent client. Use
`http://<your ip>:6969/announce` as an alternative when UDP is unavailable.
Add only one retracker URL per torrent: both listeners share the same peer
storage, so adding both protocols does not provide another peer source.
HTTP and UDP can use the same port number because they use separate transports.
UDP is disabled unless `-u`,
`RETRACKER_UDP_LISTEN`, or the YAML `udp_listen` setting enables it.

For example, `config.yml` can enable both listeners:

```yaml
listen: ":6969"
udp_listen: ":6969"
```

Run it with `retracker -c config.yml`. Allow inbound TCP **and** UDP port 6969
through the host firewall, and forward both protocols on your router when
clients connect from outside your network. The HTTP NGINX example below
proxies HTTP traffic; UDP clients must reach the UDP listener directly or
through a separately configured UDP proxy.

### HTTP only (alternative)
```bash
retracker -l :6969 -d
```
Announce URL: `http://<your ip>:6969/announce`

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
UDP forwarding works independently of the inbound UDP listener: HTTP clients
can use UDP upstream trackers even when `-u` is omitted.

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
# Run from the repository root (the build needs the root Go module).
docker build -f docker/Dockerfile -t retracker .
docker run -d --name retracker \
  -p 6969:6969/tcp -p 6969:6969/udp \
  -e RETRACKER_UDP_LISTEN=:6969 \
  retracker
```

To use the published image, replace `retracker` on the final line with
`ghcr.io/fl0v/retracker:latest`. Publishing the UDP port alone does not enable
the listener; set the environment variable or pass `-u :6969`.

### Docker Compose (recommended)
```bash
docker compose -f docker/docker-compose.yml up -d --build
```
Run from the repository root. The supplied Compose file publishes TCP and UDP
port 6969 and enables the UDP listener with `RETRACKER_UDP_LISTEN=:6969`.
Its health check tests HTTP availability; it does not probe the UDP listener.

#### Environment Variables

All environment variables override config file settings. Priority: CLI flags > Environment variables > Config file > Defaults.

Only explicitly supplied flags override other settings. For example, YAML `listen: ":7000"` stays in effect without `-l`, while `-l :6969` overrides it. `RETRACKER_DEBUG=false` disables YAML debug logging, and an explicitly empty `RETRACKER_UDP_LISTEN` disables UDP. Invalid nonempty debug environment values cause a startup error.

`-c` overrides `RETRACKER_CONFIG`; `-c=` skips the config file. `-f` overrides `RETRACKER_FORWARDS`; `-f=` disables loading forwarders. These environment variables work for both the standalone binary and the container. The container accepts flags directly, such as `docker run --rm ghcr.io/fl0v/retracker:latest -l :7000 -d=false`.

**Basic Configuration:**
- `RETRACKER_CONFIG` → `-c` - Path to configuration file (YAML)
- `RETRACKER_LISTEN` → `-l` - HTTP listen address:port (e.g., `:6969`)
- `RETRACKER_UDP_LISTEN` → `-u` - UDP listen address:port (empty to disable, e.g., `:6969`)
- `RETRACKER_DEBUG` → `-d` - Enable debug mode (`true`/`false`)

**Forwarders:**
- `RETRACKER_FORWARDS` → `-f` - Path to forwarders YAML file
- `RETRACKER_UPDATE_FORWARDERS` - Regenerate forwarders file on container start (`true`/`false`/`1`/`yes`/`on`/`enable`/`enabled`). Writes to `RETRACKER_FORWARDS` path or default `configs/forwarders.yml`
- `TRACKER_LISTS_FILE` - Path to tracker lists file used by `update-forwarders.sh` (default: `configs/trackers-lists.txt`)
- `ADDITIONAL_TRACKERS_FILE` - Path to additional trackers file used by `update-forwarders.sh` (default: `configs/trackers.txt`)

## From source

The tracker handles SIGINT and SIGTERM. Active HTTP requests get up to five seconds to finish; forwarding requests/retries and background cleanup are then canceled and joined. Container stop uses this same shutdown path. HTTP connections have a five-second header timeout and a sixty-second idle timeout.

```bash
git clone https://github.com/fl0v/retracker.git
cd retracker
go build ./cmd/retracker
./retracker -l :6969 -d
```

