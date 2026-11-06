//go:build windows

package sandbox

// defaultEnvAllow 是环境变量白名单——别把主机的密钥泄给模型执行的命令。
//
// Windows 的清单和 unix 不一样：少几个 POSIX 变量，多几个**Windows 上真的
// 需要**的：
//
//   - SystemRoot / windir / SystemDrive / COMSPEC：cmd.exe 与一批系统 DLL
//     靠它们定位目录，缺了会出怪问题（找不到 cmd.exe 的内部路径等）；
//   - PATHEXT：cmd.exe 靠它解析 .bat/.cmd 脚本，缺了 `foo.bat` 直接不认；
//   - TEMP / TMP / USERPROFILE / APPDATA / LOCALAPPDATA / PROGRAMDATA：
//     工具链的默认落点（npm、pip、go env 之类）。
//
// Go 的 os.LookupEnv 在 Windows 上是**大小写不敏感**的，写哪种大小写都行。
var defaultEnvAllow = []string{
	"PATH", "PATHEXT",
	"SystemRoot", "SystemDrive", "windir", "COMSPEC",
	"TEMP", "TMP",
	"USERPROFILE", "HOMEDRIVE", "HOMEPATH", "USERNAME", "USERDOMAIN",
	"APPDATA", "LOCALAPPDATA", "PROGRAMDATA",
	"ProgramFiles", "ProgramFiles(x86)", "ProgramW6432",
	"NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE", "COMPUTERNAME",
}
