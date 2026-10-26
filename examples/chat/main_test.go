package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 应用层的策略文件加载：读文件 + 严格解析。框架不碰这两件事，所以覆盖在这里。
func TestLoadPolicy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	body := "allow:\n  - current_time\n  - web_search\ndeny:\n  - shell\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadPolicy(path)
	if err != nil {
		t.Fatalf("loadPolicy: %v", err)
	}
	if len(cfg.Allow) != 2 || cfg.Allow[0] != "current_time" || cfg.Allow[1] != "web_search" {
		t.Fatalf("allow 解析错误: %v", cfg.Allow)
	}
	if len(cfg.Deny) != 1 || cfg.Deny[0] != "shell" {
		t.Fatalf("deny 解析错误: %v", cfg.Deny)
	}
}

// 字段名拼错必须报错：静默忽略会让用户以为规则生效了，实际上什么都没拦。
func TestLoadPolicyRejectsUnknownField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(path, []byte("alow:\n  - current_time\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPolicy(path); err == nil {
		t.Fatal("字段名拼错应报错")
	}
}

// 文件缺失/不可读：报错而不是静默降级成「无限制」。
func TestLoadPolicyMissingFile(t *testing.T) {
	if _, err := loadPolicy(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("文件缺失应报错")
	}
}

// -session 要能**继续指定的那个**会话：id 落到 data/<id>.jsonl，恢复后
// 带历史（事件数 > 0）；不存在的 id 则新建同名空会话。
func TestMustSessionByID(t *testing.T) {
	dir := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(old)

	jl, sess := mustSession(false, "my-task")
	if sess.ID != "my-task" {
		t.Fatalf("session id = %q, want my-task", sess.ID)
	}
	if _, err := os.Stat(filepath.Join(dir, "data", "my-task.jsonl")); err != nil {
		t.Fatalf("会话文件应落盘: %v", err)
	}
	jl.Close()

	// 再次打开同一 id：恢复（不新建）
	_, sess2 := mustSession(false, "my-task")
	if sess2.ID != "my-task" {
		t.Fatalf("再次打开 id = %q", sess2.ID)
	}
}

// -new 与 -session 互斥：一个要全新、一个要指定，不能猜。
func TestMustSessionFreshAndIDConflict(t *testing.T) {
	dir := t.TempDir()
	old, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(old)

	// mustSession 内部会 os.Exit（check），这里用子进程验证退出行为。
	if os.Getenv("MOS_TEST_CONFLICT") == "1" {
		mustSession(true, "x")
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestMustSessionFreshAndIDConflict")
	cmd.Env = append(os.Environ(), "MOS_TEST_CONFLICT=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("应当以非零退出码结束, out=%s", out)
	}
	if !strings.Contains(string(out), "不能同时给") {
		t.Fatalf("错误信息应说明互斥, out=%s", out)
	}
}

func TestListSessionIDs(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"b.jsonl", "a.jsonl", "notjson.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "sub.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}
	ids := listSessionIDs(dir)
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("ids = %v, want [a b]", ids)
	}
}
