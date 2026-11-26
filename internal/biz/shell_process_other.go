//go:build !unix

package biz

import "os/exec"

// configureShellProcessGroup 在非 Unix 平台退化为默认行为。
func configureShellProcessGroup(*exec.Cmd) {}

// killShellProcessGroup 在非 Unix 平台无法定向回收进程组。
func killShellProcessGroup(*exec.Cmd) error { return nil }
