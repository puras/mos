package assistant

import "testing"

// TestFileAccessConfigDefaults 验证 FileAccessConfig 零值符合 fail-closed 预期：
// WorkspaceRoot 为空、Jail* 为 false、WritableExtra 为 nil。
// 校验逻辑（拒绝 WorkspaceRoot=="" 启动）由 Task 10 的 assistant.New 落地。
func TestFileAccessConfigDefaults(t *testing.T) {
	var cfg FileAccessConfig

	if cfg.WorkspaceRoot != "" {
		t.Errorf("零值 WorkspaceRoot 应为空，got=%q", cfg.WorkspaceRoot)
	}
	if cfg.JailReads {
		t.Errorf("零值 JailReads 应为 false，got=true")
	}
	if cfg.JailWrites {
		t.Errorf("零值 JailWrites 应为 false，got=true")
	}
	if cfg.WritableExtra != nil {
		t.Errorf("零值 WritableExtra 应为 nil，got=%v", cfg.WritableExtra)
	}
}

// TestConfigHasFileAccessField 验证 Config 上已挂载 FileAccess 字段，
// 且支持按名字段赋值与读取。
func TestConfigHasFileAccessField(t *testing.T) {
	cfg := Config{FileAccess: FileAccessConfig{
		WorkspaceRoot: "/x",
		JailReads:     true,
		JailWrites:    true,
		WritableExtra: []string{"/tmp/build"},
	}}

	if cfg.FileAccess.WorkspaceRoot != "/x" {
		t.Errorf("FileAccess.WorkspaceRoot 未生效，got=%q", cfg.FileAccess.WorkspaceRoot)
	}
	if !cfg.FileAccess.JailReads || !cfg.FileAccess.JailWrites {
		t.Errorf("FileAccess.Jail* 未生效，got=%+v", cfg.FileAccess)
	}
	if len(cfg.FileAccess.WritableExtra) != 1 || cfg.FileAccess.WritableExtra[0] != "/tmp/build" {
		t.Errorf("FileAccess.WritableExtra 未生效，got=%v", cfg.FileAccess.WritableExtra)
	}
}
