package macosprobe

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestLaunchd_OwnerDisconnect(t *testing.T) {
	for _, domain := range []string{"user", "gui"} {
		t.Run(domain, func(t *testing.T) {
			probe := startLaunchdProbe(t, domain)
			if err := probe.guard.conn.Close(); err != nil {
				t.Fatal(err)
			}
			probe.waitExited(t, probe.guard.pid, probe.worker.pid, probe.grandchild.pid)
			probe.assertEscapedAlive(t)
		})
	}
}

func TestLaunchd_GuardKilled(t *testing.T) {
	for _, domain := range []string{"user", "gui"} {
		t.Run(domain, func(t *testing.T) {
			probe := startLaunchdProbe(t, domain)
			if err := unix.Kill(probe.guard.pid, unix.SIGKILL); err != nil {
				t.Fatal(err)
			}
			probe.waitExited(t, probe.guard.pid, probe.worker.pid, probe.grandchild.pid)
			probe.assertEscapedAlive(t)
		})
	}
}
