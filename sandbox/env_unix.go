//go:build !windows

package sandbox

// defaultEnvAllow 是环境变量白名单（PATH/HOME/LANG/…）——别把主机的密钥泄给
// 模型执行的命令。
var defaultEnvAllow = []string{"PATH", "HOME", "LANG", "LC_ALL", "TMPDIR", "PWD", "USER", "SHELL", "TERM"}
