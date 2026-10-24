module github.com/fl0v/retracker

go 1.26.0

toolchain go1.27.3

require (
	github.com/fl0v/retracker/bittorrent/common v0.0.0
	github.com/fl0v/retracker/bittorrent/response v0.0.0
	github.com/fl0v/retracker/bittorrent/tracker v0.0.0
	github.com/fl0v/retracker/common v0.0.0
	github.com/prometheus/client_golang v1.25.0
	github.com/zeebo/bencode v1.0.0
	gopkg.in/yaml.v2 v2.4.0
)

replace (
	github.com/fl0v/retracker/bittorrent/common => ./bittorrent/common
	github.com/fl0v/retracker/bittorrent/response => ./bittorrent/response
	github.com/fl0v/retracker/bittorrent/tracker => ./bittorrent/tracker
	github.com/fl0v/retracker/common => ./common
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/klauspost/compress v1.20.3 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/prometheus/client_model v0.6.3 // indirect
	github.com/prometheus/common v0.72.1 // indirect
	github.com/prometheus/procfs v0.23.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)
