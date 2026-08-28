package fs

import (
	"errors"
	"strings"
	"testing"
)

func TestPathDeniedErrorMessage(t *testing.T) {
	err := &PathDeniedError{
		Display:   "../../etc/passwd",
		Target:    "/etc/passwd",
		Workspace: "/home/user/proj",
		Op:        OpRead,
		Mode:      ModeLocalJailed,
	}
	msg := err.Error()
	if !strings.Contains(msg, "../../etc/passwd") {
		t.Errorf("错误文案缺原始路径: %s", msg)
	}
	if !strings.Contains(msg, "local-jailed") {
		t.Errorf("错误文案缺模式: %s", msg)
	}
}

func TestNotConfiguredErrorMessage(t *testing.T) {
	err := &NotConfiguredError{}
	if !strings.Contains(err.Error(), "filesystem") {
		t.Errorf("错误文案缺关键字: %s", err.Error())
	}
}

func TestErrorsAreDistinct(t *testing.T) {
	var pd *PathDeniedError
	var nc *NotConfiguredError
	if !errors.As(&PathDeniedError{Display: "x", Workspace: "/y", Op: OpRead, Mode: ModeLocalJailed}, &pd) {
		t.Fatal("PathDeniedError 应被 errors.As 识别")
	}
	if !errors.As(&NotConfiguredError{}, &nc) {
		t.Fatal("NotConfiguredError 应被 errors.As 识别")
	}
}
