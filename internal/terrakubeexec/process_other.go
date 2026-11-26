//go:build !linux

package terrakubeexec

import "errors"

func protectResolverProcess() error {
	return errors.New("run the trusted Configs deployment on its Linux Terrakube executor")
}
