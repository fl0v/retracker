# retracker

![Architecture Diagram](docs/diagram.jpg)

Lightweight BitTorrent tracker (HTTP + UDP, BEP 15) in a single Go binary. Everything is in-memory; no database or external web stack required.

This is built with extensive help from different AI agents, as an experiment.

## Features (concise)
- HTTP and UDP announce/scrape with shared core logic (BEP 15 compliant)
- Forwarding to external trackers (HTTP/HTTPS/UDP) with peer aggregation
- Adaptive worker pool with rate limiting and throttling for forwarders
- Prometheus metrics endpoint
- IPv4/IPv6 support and secure connection ID handling for UDP

## Quick start
```bash
go install github.com/fl0v/retracker@latest
retracker -l :6969 -d
```
Add `http://<your ip>:6969/announce` to your torrent.

Full installation and deployment options are in [INSTALL.md](INSTALL.md).

## Usage highlights
- Enable UDP alongside HTTP:
  ```bash
  retracker -l :6969 -u :6969 -d
  ```
- Load forwarders and aggregate peers:
  ```bash
  retracker -l :6969 -d -f forwarders.yml
  ```
- Prometheus metrics: `GET /metrics` (when enabled in config)

## Docker
- Build: `cd docker && docker build -t retracker .`
- Run: `docker run -d -p 6969:6969 retracker`
- Compose: `cd docker && docker-compose up -d`

See [INSTALL.md](INSTALL.md) for configuration env vars and examples.

## Development
- Format: `make fmt` (gofumpt)
- Lint: `make lint` (golangci-lint)
- All checks: `make check`

## License

MIT License - see [LICENSE](LICENSE).
