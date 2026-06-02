//go:build windows
package oracode

import (
	"os/exec"
	"syscall"
)

func init() {
	hideWindow = func(cmd *exec.Cmd) {
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	}
	killExistingLlamaServers = func() {
		cmd := exec.Command("taskkill", "/f", "/im", "llama-server.exe")
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		_ = cmd.Run()
	}
}
