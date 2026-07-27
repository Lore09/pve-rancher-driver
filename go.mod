module github.com/lore09/pve-rancher-driver

go 1.22

require (
	github.com/docker/machine v0.16.2
	github.com/luthermonson/go-proxmox v0.2.0
)

require (
	github.com/Azure/go-ansiterm v0.0.0-20250102033503-faa5f7b0171c // indirect
	github.com/buger/goterm v1.0.4 // indirect
	github.com/diskfs/go-diskfs v1.2.0 // indirect
	github.com/gorilla/websocket v1.4.2 // indirect
	github.com/jinzhu/copier v0.3.4 // indirect
	github.com/magefile/mage v1.14.0 // indirect
	golang.org/x/crypto v0.0.0-20191011191535-87dc89f01550 // indirect
	golang.org/x/sys v0.0.0-20210630005230-0f9fa26af87c // indirect
	gopkg.in/airbrake/gobrake.v2 v2.0.9 // indirect
	gopkg.in/djherbis/times.v1 v1.2.0 // indirect
	gopkg.in/gemnasium/logrus-airbrake-hook.v2 v2.1.2 // indirect
)

// docker/machine v0.16.2 imports `github.com/docker/docker/pkg/term`, which
// was removed from modern docker/docker releases. Pin to the last
// incompatible version that still ships the package, along with the
// capital-S Sirupsen/logrus import it expects.
require (
	github.com/Sirupsen/logrus v1.0.5 // indirect
	github.com/docker/docker v1.13.1+incompatible // indirect
)

replace github.com/Sirupsen/logrus => github.com/sirupsen/logrus v1.0.5
