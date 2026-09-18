//go:build windows

package radio

import (
	"os/exec"
	"syscall"
)

func hideSRSExec(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000,
	}
}
