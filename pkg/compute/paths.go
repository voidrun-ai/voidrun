package compute

import "fmt"

// InstancesRoot is set at daemon startup from config.
var InstancesRoot string

func SetInstancesRoot(path string) {
	if path != "" {
		InstancesRoot = path
	}
}

func GetInstanceDir(id string) string {
	return fmt.Sprintf("%s/%s", InstancesRoot, id)
}

func GetSocketPath(id string) string {
	return fmt.Sprintf("%s/%s/vm.sock", InstancesRoot, id)
}

func GetVsockPath(id string) string {
	return fmt.Sprintf("%s/%s/vsock.sock", InstancesRoot, id)
}

func GetPIDPath(id string) string {
	return fmt.Sprintf("%s/%s/vm.pid", InstancesRoot, id)
}

func GetLogPath(id string) string {
	return fmt.Sprintf("%s/%s/vm.log", InstancesRoot, id)
}

func GetEventPath(id string) string {
	return fmt.Sprintf("%s/%s/vm.evt", InstancesRoot, id)
}

func GetEventOffsetPath(id string) string {
	return fmt.Sprintf("%s/%s/vm.evt_offset", InstancesRoot, id)
}

func GetOverlayPath(id string) string {
	return fmt.Sprintf("%s/%s/overlay.qcow2", InstancesRoot, id)
}

func GetRawOverlayPath(id string) string {
	return fmt.Sprintf("%s/%s/overlay.raw", InstancesRoot, id)
}

func GetSnapshotBaseDir(id string) string {
	return fmt.Sprintf("%s/%s/snapshots", InstancesRoot, id)
}

// FC jailer API socket on the host (outside chroot view).
func GetFCAPISocketPath(chrootBase, id string) string {
	return fmt.Sprintf("%s/firecracker/%s/root/run/firecracker.socket", chrootBase, id)
}

func GetFCChrootDir(chrootBase, id string) string {
	return fmt.Sprintf("%s/firecracker/%s/root", chrootBase, id)
}

func GetFCPIDPath(chrootBase, id string) string {
	return fmt.Sprintf("%s/firecracker/%s/root/firecracker.pid", chrootBase, id)
}
