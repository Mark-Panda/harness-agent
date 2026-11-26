//go:build unix

package biz

import (
	"errors"
	"os/exec"
	"syscall"
)

// configureShellProcessGroup 让命令在独立进程组中运行，取消时可回收全部子进程。
func configureShellProcessGroup(command *exec.Cmd) {
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.Setpgid = true
}

// killShellProcessGroup 杀死整个进程组，避免后台子进程在命令结束后继续运行。
func killShellProcessGroup(command *exec.Cmd) error {
	if command.Process == nil {
		return nil
	}
	err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
