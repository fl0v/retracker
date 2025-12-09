# retracker

![Architecture Diagram](docs/diagram.jpg)

Simple HTTP and UDP torrent tracker.

* Keep all in memory (no persistent; doesn't require a database).
* Single binary executable (doesn't require a web-backend [apache, php-fpm, uwsgi, etc.])
* **HTTP and UDP tracker protocol support** (BEP 15 compliant)
* **Decoupled announce logic** - HTTP and UDP handlers share the same core announce processing
* **Forward announces to external trackers** - Supports both HTTP and UDP forwarders (BEP 15)
* **Peer aggregation** - Combines local peers with peers from external trackers
* Expose some metrics for Prometheus monitoring

## Installing

```
go install 'github.com/fl0v/retracker@latest'
```
> Executables are installed in the directory named by the GOBIN environment variable, which defaults to $GOPATH/bin or $HOME/go/bin if the GOPATH environment variable is not set. Executables in $GOROOT are installed in $GOROOT/bin or $GOTOOLDIR instead of $GOBIN.

## Usage
### Standalone

Start tracker on port 8080 with debug mode.
```
retracker -l :8080 -d
```
Add http://\<your ip>:8080/announce to your torrent.

### Standalone with UDP support

Start tracker with both HTTP and UDP support:
```
retracker -l :8080 -u :8080 -d
```
* `-l :8080` - HTTP tracker on port 8080
* `-u :8080` - UDP tracker on port 8080

The tracker supports both protocols simultaneously. UDP support includes:
* **BEP 15 compliant** UDP tracker protocol
* **IPv4 and IPv6 support** - automatically detects address family
* **Connection ID management** - secure random connection IDs with automatic cleanup
* **Connect, Announce, and Scrape** operations
* **Decoupled architecture** - UDP and HTTP share the same `ProcessAnnounce` logic for consistent behavior

Both HTTP and UDP announce handlers use the same decoupled announce processing logic, ensuring consistent peer management and forwarding behavior regardless of the transport protocol.

## Behind NGINX
Configure nginx like:
```
# cat /etc/nginx/sites-enabled/retracker.local
server {
        listen 80;

        server_name retracker.local;

        access_log /var/log/nginx/retracker.local-access.log;

        proxy_set_header X-Real-IP $remote_addr;

        location /metrics {
                allow 10.0.0.0/8;
                deny  all;
                proxy_pass http://localhost:8080;
        }

        location / {
                proxy_pass http://localhost:8080;
        }
}
```

Start tracker on port 8080 with getting remote address from X-Real-IP header.
```
retracker -l :8080 -x -p
```

Add retracker.local to your local DNS or /etc/hosts.

Add http://retracker.local/announce to your torrent.

### Standalone with announce forwarding

You can forward announce requests to external trackers (HTTP or UDP) and aggregate peers from them with your local peers.

```
retracker -l :8080 -d -f forwarders.yml
```

**forwarders.yml** format:
```yaml
# HTTP trackers
- uri: http://1.2.3.4:8080/announce
- uri: https://tracker.example.com/announce

# UDP trackers (BEP 15)
- uri: udp://tracker.example.com:6969/announce

# With optional IP override (useful when behind NAT)
- uri: http://5.6.7.8:8080/announce
  ip: 192.168.1.15

# With custom host header
- uri: http://192.168.1.1:8080/announce
  host: retracker.local

# With custom name for logs/stats
- uri: http://tracker.example.com/announce
  name: my-tracker
```

**Features:**
- Supports both HTTP/HTTPS and UDP forwarders (BEP 15 compliant)
- Automatic protocol detection from URI scheme
- Peer aggregation from multiple forwarders
- Connection ID management for UDP forwarders
- Automatic retry with exponential backoff
- Worker pool for parallel forwarding

**Auto-generating forwarders.yml:**
```bash
scripts/update-forwarders.sh
```
This script downloads tracker lists from public sources and generates a `forwarders.yml` file with both HTTP and UDP trackers.

Add http://\<your ip>:8080/announce to your torrent.

## Docker Setup

### Using Dockerfile

Build the Docker image:
```bash
cd docker
docker build -t retracker .
```

Run the container:
```bash
docker run -d -p 6969:6969 retracker
```

### Using Docker Compose

The easiest way to run retracker with Docker is using the provided `docker-compose.yml`:

```bash
cd docker
docker-compose up -d
```

This will:
* Build the retracker image
* Start the container on port 6969
* Mount the forwarders configuration from `../configs/forwarders.yml`

To customize the configuration, edit `docker/docker-compose.yml` and modify the environment variables:
* `RETRACKER_CONFIG` - Path to configuration file (maps to `-c` flag)
* `RETRACKER_LISTEN` - HTTP listen address:port (maps to `-l` flag)
* `RETRACKER_UDP_LISTEN` - UDP listen address:port (maps to `-u` flag, optional)
* `RETRACKER_FORWARDS` - Path to forwarders configuration file (maps to `-f` flag)
* `RETRACKER_DEBUG` - Enable debug mode (maps to `-d` flag, set to `true` to enable)

All other configuration options should be set in the config file specified by `RETRACKER_CONFIG`.

Example with UDP support:
```yaml
environment:
  - RETRACKER_LISTEN=:6969
  - RETRACKER_UDP_LISTEN=:6969
  - RETRACKER_FORWARDS=/app/forwarders.yml
  - RETRACKER_DEBUG=false
```

## Development

### Code Formatting and Linting

This project uses:
- **gofumpt** - Stricter gofmt for code formatting
- **golangci-lint** - Comprehensive Go linter

#### Setup

Install the tools:
```bash
# Install gofumpt
go install mvdan.cc/gofumpt@latest

# Install golangci-lint
go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
```

#### Usage

Format code:
```bash
make fmt
```

Check formatting:
```bash
make fmt-check
```

Run linter:
```bash
make lint
```

Run linter with auto-fix:
```bash
make lint-fix
```

Run all checks:
```bash
make check
```

### Project Structure

The project follows the [Standard Go Project Layout](https://github.com/golang-standards/project-layout):

```
retracker/
├── cmd/retracker/          # Application entry point
├── internal/               # Private application code
│   ├── config/            # Configuration handling
│   ├── server/            # Server implementation
│   └── observability/     # Metrics and monitoring
├── bittorrent/            # BitTorrent protocol modules
│   ├── common/            # Common types
│   ├── response/          # Response handling
│   └── tracker/           # Tracker protocol
├── common/                # Shared common module
├── scripts/               # Build and utility scripts
├── configs/               # Runtime configuration files
└── docs/                  # Documentation
```

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details
