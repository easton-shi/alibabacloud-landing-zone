//go:build !windows

package control

import "syscall"

func execSelf(executable string, argv, env []string) error {
	return syscall.Exec(executable, append([]string{executable}, argv...), env)
}
