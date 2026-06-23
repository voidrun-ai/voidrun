package firecracker

import (
	"testing"

	"voidrun/pkg/compute"
)

func TestJailPathsFor(t *testing.T) {
	compute.SetHostConfig(compute.HostConfig{
		InstancesDir: "/var/lib/voidrun/instances",
		FCChrootBase: "/var/lib/voidrun/jails",
	})
	jp := jailPathsFor("abc123")
	want := "/var/lib/voidrun/jails/firecracker/abc123/root/run/firecracker.socket"
	if jp.apiSocket != want {
		t.Fatalf("api socket = %q want %q", jp.apiSocket, want)
	}
}

func TestChrootBaseDirDefault(t *testing.T) {
	compute.SetHostConfig(compute.HostConfig{InstancesDir: "/data/instances"})
	if got := chrootBaseDir(); got != "/data/instances/jails" {
		t.Fatalf("got %q", got)
	}
}
