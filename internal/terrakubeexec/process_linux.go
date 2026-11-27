package terrakubeexec

import "syscall"

const prSetDumpable = 4

// The deployment template replaces its Bash process with this binary.
// Disable same-user access to the executor's initial environment in Linux procfs.
func protectResolverProcess() error {
	_, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, prSetDumpable, 0, 0, 0, 0, 0)
	if errno != 0 {
		return executionError("protect the workspace 1Password token from subprocesses", errno)
	}

	return nil
}
