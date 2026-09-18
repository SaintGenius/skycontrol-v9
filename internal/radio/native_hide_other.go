//go:build !windows

package radio

import "os/exec"

func hideSRSExec(cmd *exec.Cmd) {}
