package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"strings"
	"time"
)

type HostProbe interface {
	LookPath(string) (string, error)
	Stat(string) error
	ReadFile(string) ([]byte, error)
}

type osHostProbe struct{}

func (osHostProbe) LookPath(name string) (string, error) { return exec.LookPath(name) }
func (osHostProbe) Stat(path string) error               { _, err := os.Stat(path); return err }
func (osHostProbe) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

type HostDetectionOptions struct {
	Probe             HostProbe
	PodmanBinary      string
	ContainerdSocket  string
	CgroupControllers string
	UserNSControl     string
	SubUIDFile        string
	SubGIDFile        string
	Username          string
}

func (o HostDetectionOptions) withDefaults() HostDetectionOptions {
	if o.Probe == nil {
		o.Probe = osHostProbe{}
	}
	if o.ContainerdSocket == "" {
		o.ContainerdSocket = "/run/containerd/containerd.sock"
	}
	if strings.TrimSpace(o.PodmanBinary) == "" {
		o.PodmanBinary = "podman"
	}
	if o.CgroupControllers == "" {
		o.CgroupControllers = "/sys/fs/cgroup/cgroup.controllers"
	}
	if o.UserNSControl == "" {
		o.UserNSControl = "/proc/sys/kernel/unprivileged_userns_clone"
	}
	if o.SubUIDFile == "" {
		o.SubUIDFile = "/etc/subuid"
	}
	if o.SubGIDFile == "" {
		o.SubGIDFile = "/etc/subgid"
	}
	if o.Username == "" {
		if current, err := user.Current(); err == nil {
			o.Username = current.Username
		}
	}
	return o
}

// DetectHostPrerequisites is read-only: it checks paths and reads kernel
// settings, but never connects to a runtime socket, starts a process, or
// creates a container.
func DetectHostPrerequisites(ctx context.Context, backend BackendKind, options HostDetectionOptions) (ReadinessReport, error) {
	if err := ctx.Err(); err != nil {
		return ReadinessReport{}, err
	}
	o := options.withDefaults()
	report := ReadinessReport{Backend: backend, CheckedAt: time.Now().UTC()}
	add := func(name string, needed, passed bool, detail string) {
		report.Checks = append(report.Checks, Check{Name: name, Needed: needed, Passed: passed, Detail: detail})
	}
	if runtime.GOOS != "linux" {
		add("linux", true, false, "sandbox backends require Linux")
		return finalizeReadiness(report), nil
	}
	add("linux", true, true, runtime.GOOS)
	_, containerdErr := o.Probe.LookPath("containerd")
	_, runscErr := o.Probe.LookPath("runsc")
	_, podmanErr := o.Probe.LookPath(o.PodmanBinary)
	cgroupErr := o.Probe.Stat(o.CgroupControllers)
	userNSValue, userNSErr := o.Probe.ReadFile(o.UserNSControl)
	userNSOK := userNSErr == nil && strings.TrimSpace(string(userNSValue)) == "1"

	switch backend {
	case BackendContainerdRunsc:
		add("containerd", true, containerdErr == nil, errDetail(containerdErr))
		add("runsc", true, runscErr == nil, errDetail(runscErr))
		add("cgroups-v2", true, cgroupErr == nil, errDetail(cgroupErr))
		add("unprivileged-user-namespaces", true, userNSOK, errDetail(userNSErr))
		// Stat is intentionally the only socket operation. A readable path does
		// not prove connectivity and this function never opens it.
		socketErr := o.Probe.Stat(o.ContainerdSocket)
		add("containerd-socket-present", true, socketErr == nil, errDetail(socketErr))
	case BackendPodman:
		add("podman", true, podmanErr == nil, errDetail(podmanErr))
		add("cgroups-v2", true, cgroupErr == nil, errDetail(cgroupErr))
		add("unprivileged-user-namespaces", true, userNSOK, errDetail(userNSErr))
		subUID, subUIDErr := o.Probe.ReadFile(o.SubUIDFile)
		subGID, subGIDErr := o.Probe.ReadFile(o.SubGIDFile)
		subUIDOK := subordinateRangePresent(subUID, o.Username)
		subGIDOK := subordinateRangePresent(subGID, o.Username)
		add("subordinate-uids", true, subUIDErr == nil && subUIDOK, subordinateDetail(subUIDErr, subUIDOK, o.Username))
		add("subordinate-gids", true, subGIDErr == nil && subGIDOK, subordinateDetail(subGIDErr, subGIDOK, o.Username))
	default:
		return ReadinessReport{}, fmt.Errorf("unsupported backend %q", backend)
	}
	return finalizeReadiness(report), nil
}

func subordinateRangePresent(contents []byte, username string) bool {
	if strings.TrimSpace(username) == "" {
		return false
	}
	for _, line := range strings.Split(string(contents), "\n") {
		fields := strings.Split(strings.TrimSpace(line), ":")
		if len(fields) != 3 || fields[0] != username {
			continue
		}
		var start, count uint64
		if _, err := fmt.Sscanf(fields[1]+" "+fields[2], "%d %d", &start, &count); err == nil && start > 0 && count >= 65536 {
			return true
		}
	}
	return false
}

func subordinateDetail(err error, present bool, username string) string {
	if err != nil {
		return errDetail(err)
	}
	if present {
		return "range present for " + username
	}
	if username == "" {
		return "current username unavailable"
	}
	return "no range of at least 65536 IDs for " + username
}

func finalizeReadiness(report ReadinessReport) ReadinessReport {
	report.Ready = true
	if report.Backend == BackendContainerdRunsc {
		report.IsolationGrade = IsolationEnhanced
	} else {
		report.IsolationGrade = IsolationStandard
	}
	for _, check := range report.Checks {
		if check.Needed && !check.Passed {
			report.Ready = false
		}
	}
	return report
}

func errDetail(err error) string {
	if err == nil {
		return "present"
	}
	if errors.Is(err, os.ErrNotExist) {
		return "not found"
	}
	return err.Error()
}
