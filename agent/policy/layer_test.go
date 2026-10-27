package policy

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// forbiddenImports 是框架层不允许的依赖：框架不碰文件系统、网络，也不绑定配置格式。
//
// 理由不是洁癖，是三条实打实的约束：
//   - 框架替宿主打开路径，等于绕过 S5d 的文件 jail——框架自己成了越界的那一方；
//   - 部署形态未必有文件系统（配置在配置中心/远端下发/DB），os.Read 直接失效；
//   - 隐式 I/O 难以替换：宿主想从 HTTP/DB 取配置时无从下手。
//
// 需要 I/O 的能力一律以接口注入，由宿主选实现（如 session.Persistence）。
// 配置格式同理归宿主：框架不 import yaml，Config 不带格式 tag——平台可能用
// YAML、JSON、配置中心或远端下发，绑一个就要为它背依赖。
//
// 注意：**encoding/json 不在禁令内**——那是框架自己的载荷格式（ToolCall.Args
// 的解析要用它），与“配置格式归宿主”是两回事。
//
// 若将来确有必要在框架内支持某种配置格式，请连同理由删掉本断言（显式放宽，
// 不要悄悄绕过）。
func TestFrameworkDoesNotTouchFilesystem(t *testing.T) {
	forbidden := map[string]bool{
		"os":            true,
		"path/filepath": true,
		"io/ioutil":     true,
		"net/http":      true,
		"database/sql":  true,
		"net":           true,

		"gopkg.in/yaml.v3": true,
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("解析 %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("%s: 非法 import %s", name, imp.Path.Value)
			}
			if forbidden[path] {
				t.Errorf("%s 引入了 %q：框架层不碰文件系统/网络，"+
					"配置以字节进、由宿主决定字节从哪来", name, path)
			}
		}
	}
}
