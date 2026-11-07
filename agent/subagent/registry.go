package subagent

import (
	"strings"

	"github.com/puras/mos/tool"
)

// ChildRegistry 按「声明白名单 ∩ 父可见工具集」求交，给子代理造一个
// **受限注册表**。
//
// 这是白名单真正落地的地方（S8.5-B）：子代理拿到的是**新注册表**，里面
// 只有交集里的那些工具——子代理内部调用工具时，父注册表里它拿不到的工具
// 一律不存在（不是「调用了被拒」，是压根没这个工具）。
//
// **deny 优先**：父可见集已经过了宿主 policy 与可见性层，装配层传进来的
// 就是「模型当前可见的工具名」——被 policy 挡掉的工具**不在**父可见集里，
// 声明也拿不到它（声明不能放大宿主策略；DSH 同款：toolFilter + 父策略
// 向下委派）。
//
// 顺带口径：父注册表里 **deferred 但未激活**的工具也不进子注册表——
// 子代理工具面不经 tool_search（2026-11-06 拍板 ⑥），子代理只继承父
// **当前**能用的工具。
//
// 第二个返回值是**被挡掉的声明白名单项**（诊断用：告诉宿主「你声明的
// shell 被宿主策略挡了」）；声明白名单为空时没有挡掉项。
func ChildRegistry(parent *tool.Registry, declared []string) (*tool.Registry, []string) {
	child := tool.NewRegistry()
	if parent == nil {
		return child, append([]string(nil), declared...)
	}

	visible := parent.Visible()
	allowed := ResolveTools(declared, visible)
	allowedSet := toSet(allowed)
	for _, name := range allowed {
		t, ok := parent.Get(name)
		if !ok {
			continue
		}
		// 注册表内名字唯一，重复注册不可能发生；真发生了也只能跳过
		// （不能因为一个工具注册失败就丢掉整个子注册表）。
		_ = child.Register(t)
	}

	var dropped []string
	for _, name := range declared {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := allowedSet[name]; !ok {
			dropped = append(dropped, name)
		}
	}
	return child, dropped
}
