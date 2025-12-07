module github.com/fl0v/retracker

go 1.22.0

toolchain go1.22.4

require (
	github.com/fl0v/retracker/bittorrent/common v0.0.0
	github.com/fl0v/retracker/bittorrent/response v0.0.0
	github.com/fl0v/retracker/bittorrent/tracker v0.0.0
	github.com/fl0v/retracker/common v0.0.0
	github.com/prometheus/client_golang v1.20.5
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
	github.com/klauspost/compress v1.17.11 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/prometheus/client_model v0.6.1 // indirect
	github.com/prometheus/common v0.60.1 // indirect
	github.com/prometheus/procfs v0.15.1 // indirect
	github.com/rogpeppe/go-internal v1.13.1 // indirect
	golang.org/x/sys v0.27.0 // indirect
	google.golang.org/protobuf v1.35.1 // indirect
)
