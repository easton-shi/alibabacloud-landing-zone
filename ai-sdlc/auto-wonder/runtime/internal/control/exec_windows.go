//go:build windows

package control

import "errors"

func execSelf(string, []string, []string) error {
	return errors.New("in-process restart is not supported on Windows")
}
