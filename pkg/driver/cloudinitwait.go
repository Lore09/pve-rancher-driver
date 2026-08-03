package driver

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/docker/machine/libmachine/drivers"
	"github.com/docker/machine/libmachine/log"
)

// cloudInitWaitScript blocks until cloud-init has finished, then reports what
// happened on a single machine-readable line.
//
// It always exits 0 and encodes the real result in `pve-cloudinit-result=`,
// because a non-zero exit from the SSH command would be indistinguishable
// from the connection itself failing — and the two want different handling:
// one is a guest that is up but degraded, the other is a guest the driver
// cannot reach at all.
//
// `cloud-init status --wait` is the supported way to ask this question. Its
// exit code is 0 when everything ran, 2 when cloud-init finished with
// recoverable errors (a failed optional module, typically), and non-zero
// otherwise. It is run without sudo first because on most images the status
// file is world-readable; the sudo retry covers the images where it is not.
const cloudInitWaitScript = `
if ! command -v cloud-init >/dev/null 2>&1; then
  echo "pve-cloudinit-result=absent"
  exit 0
fi
cloud-init status --wait >/dev/null 2>&1
rc=$?
if [ "$rc" -ne 0 ] && [ "$rc" -ne 2 ]; then
  sudo cloud-init status --wait >/dev/null 2>&1
  rc=$?
fi
echo "pve-cloudinit-result=$rc"
cloud-init status --long 2>/dev/null | tr '\n' ' ' || true
exit 0
`

// waitForCloudInit blocks until cloud-init inside the guest reports that it
// has finished, and is what makes a machine genuinely ready rather than
// merely reachable.
//
// The problem it solves: sshd comes up early on most cloud images, well
// before cloud-init has written the resolver, installed the default route or
// created the login user. Rancher fires its first bootstrap command the
// moment SSH answers, so without this the command can run against a
// half-configured guest and fail on something transient — and a failed
// bootstrap deletes and recreates the whole machine, often in a loop.
// --pve-provision-delay only guesses at how long that takes; this waits for
// the guest to say so.
//
// A guest with no cloud-init installed is not an error: the driver logs it
// and moves on, because the template may legitimately be configured some
// other way. Cloud-init finishing *with* recoverable errors is also not an
// error — a failed optional module is common and rarely fatal to the node —
// but it is logged loudly, since it is the first thing worth looking at if
// the node later misbehaves.
func (d *Driver) waitForCloudInit() error {
	if d.CloudInitTimeout <= 0 {
		return nil
	}

	done := make(chan error, 1)
	go func() {
		log.Infof("pve: waiting for cloud-init to finish on %s (up to %s, --pve-cloudinit-timeout)", d.IPAddress, d.CloudInitTimeout)
		if err := drivers.WaitForSSH(d); err != nil {
			done <- fmt.Errorf("pve: SSH never became available to wait for cloud-init: %w", err)
			return
		}
		out, err := drivers.RunSSHCommandFromDriver(d, cloudInitWaitScript)
		if err != nil {
			done <- fmt.Errorf("pve: could not query cloud-init status: %w (guest output: %s)", err, strings.TrimSpace(out))
			return
		}
		done <- interpretCloudInitResult(out)
	}()

	select {
	case err := <-done:
		return err
	case <-time.After(d.CloudInitTimeout):
		return fmt.Errorf("pve: cloud-init did not finish within %s; raise --pve-cloudinit-timeout, or set it to 0 to hand the machine over without waiting", d.CloudInitTimeout)
	}
}

// interpretCloudInitResult turns the script's output into a driver result.
// Anything it cannot parse is treated as "cannot tell" and allowed through
// with a warning: refusing to provision because a status line was unfamiliar
// would be a worse failure than the one being guarded against.
func interpretCloudInitResult(out string) error {
	detail := strings.TrimSpace(out)
	code, ok := parseCloudInitResult(out)
	if !ok {
		log.Warnf("pve: could not read cloud-init status from the guest (output: %s); continuing", detail)
		return nil
	}
	switch code {
	case "absent":
		log.Infof("pve: guest has no cloud-init installed; skipping the wait")
		return nil
	case "0":
		log.Infof("pve: cloud-init finished successfully")
		return nil
	case "2":
		log.Warnf("pve: cloud-init finished with recoverable errors — the node should still work, but check this first if it misbehaves: %s", detail)
		return nil
	default:
		return fmt.Errorf("pve: cloud-init failed in the guest (exit %s): %s", code, detail)
	}
}

// parseCloudInitResult pulls the value out of the `pve-cloudinit-result=` line
// the script emits.
func parseCloudInitResult(out string) (string, bool) {
	const marker = "pve-cloudinit-result="
	idx := strings.Index(out, marker)
	if idx < 0 {
		return "", false
	}
	rest := out[idx+len(marker):]
	if cut := strings.IndexAny(rest, " \t\r\n"); cut >= 0 {
		rest = rest[:cut]
	}
	rest = strings.TrimSpace(rest)
	if rest == "absent" {
		return rest, true
	}
	if _, err := strconv.Atoi(rest); err != nil {
		return "", false
	}
	return rest, true
}
