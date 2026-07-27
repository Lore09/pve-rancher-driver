// Package main is the entrypoint for the docker-machine-driver-pve binary.
//
// docker-machine loads driver plugins by file name: the binary must be named
// "docker-machine-driver-pve" and is launched as a child process by
// docker-machine, communicating over a private RPC channel.
package main

import (
	"fmt"
	"os"

	"github.com/docker/machine/libmachine/drivers/plugin"

	"github.com/lore09/pve-rancher-driver/pkg/driver"
)

// These vars are stamped by goreleaser's ldflags at build time. They are not
// used by the plugin's logic — they exist so `docker-machine-driver-pve
// --version` (or a debug attach) can identify a binary by tag/commit even
// when the binary itself refuses to run outside the plugin RPC channel.
var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

func main() {
	// `--version` short-circuits before the plugin handshake so operators can
	// identify the binary even though docker-machine normally refuses to
	// invoke it standalone.
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Printf("docker-machine-driver-pve %s (commit=%s built=%s)\n", version, commit, buildDate)
		return
	}

	plugin.RegisterDriver(driver.NewDriver("", ""))
}