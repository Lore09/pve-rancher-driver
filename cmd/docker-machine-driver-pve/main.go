// Package main is the entrypoint for the docker-machine-driver-pve binary.
//
// docker-machine loads driver plugins by file name: the binary must be named
// "docker-machine-driver-pve" and is launched as a child process by
// docker-machine, communicating over a private RPC channel.
package main

import (
	"github.com/docker/machine/libmachine/drivers/plugin"

	"github.com/lore09/pve-rancher-driver/pkg/driver"
)

func main() {
	plugin.RegisterDriver(driver.NewDriver("", ""))
}