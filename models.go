package mos

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/puras/mos/llm"
)

// ModelOption 是模型目录里的一项。
//
// 目录是「会话里可切换的候选」：宿主给一份，模型可以在运行期换（下一轮生效），
// 不用重建智能体。凭据只留在 Chat 里，不外泄。
type ModelOption struct {
	// ID 是稳定标识（换模型时用它指认；同一目录内唯一）。
	ID string
	// Label 是展示名（宿主 UI 用；空 = 用 ID）。
	Label string
	// Chat 是模型对象。
	Chat llm.ChatModel
}

// switchableModel 是「当前模型」的转发壳：调用时才决定转发给谁。
//
// 换模型 = 换壳里的目标，不重建决策器——所以「框架是构造时捕获还是每轮读取
// 决策器」这种实现细节不影响换模型的正确性。
type switchableModel struct {
	mu  sync.RWMutex
	cur llm.ChatModel
}

// newSwitchableModel 造一个壳（初始目标 = 给定的模型）。
func newSwitchableModel(m llm.ChatModel) *switchableModel {
	return &switchableModel{cur: m}
}

// current 返回当前目标。
func (m *switchableModel) current() llm.ChatModel {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cur
}

// set 换目标（下一轮开始生效；正在跑的那一轮不受影响——它已经取过目标）。
func (m *switchableModel) set(next llm.ChatModel) {
	m.mu.Lock()
	m.cur = next
	m.mu.Unlock()
}

// Capabilities 实现 llm.ChatModel：如实报告**当前目标**的能力。
//
// 能力按当前模型算：换到一个不支持流式的模型之后，驱动下一次决策会看到
// 「不支持流式」——如实反映，不是撒谎。
func (m *switchableModel) Capabilities() llm.Capability {
	cur := m.current()
	if cur == nil {
		return 0
	}
	return cur.Capabilities()
}

// Chat 实现 llm.ChatModel（转发给当前目标）。
func (m *switchableModel) Chat(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	cur := m.current()
	if cur == nil {
		return nil, errors.New("mos: 没有可用的模型")
	}
	return cur.Chat(ctx, req)
}

// ChatStream 实现 llm.ChatModel（转发给当前目标）。
func (m *switchableModel) ChatStream(ctx context.Context, req *llm.Request, emit func(llm.StreamChunk) error) (*llm.Response, error) {
	cur := m.current()
	if cur == nil {
		return nil, errors.New("mos: 没有可用的模型")
	}
	return cur.ChatStream(ctx, req, emit)
}

// modelCatalog 是模型目录：ID → 选项（保序）+ 当前选择。
type modelCatalog struct {
	mu    sync.RWMutex
	byID  map[string]ModelOption
	order []string
	cur   string
	shell *switchableModel
}

// newModelCatalog 校验并装配目录。
//
// fail-closed：ID 为空、Chat 为 nil、ID 重复、默认 ID 不在目录里——一律报错。
// 「半截的目录」比「没有目录」更难查。
func newModelCatalog(options []ModelOption, defaultID string) (*modelCatalog, error) {
	byID := make(map[string]ModelOption, len(options))
	order := make([]string, 0, len(options))
	for _, opt := range options {
		if opt.ID == "" {
			return nil, errors.New("mos: 模型目录里有项没有 ID")
		}
		if opt.Chat == nil {
			return nil, fmt.Errorf("mos: 模型 %q 没有 Chat 模型", opt.ID)
		}
		if _, dup := byID[opt.ID]; dup {
			return nil, fmt.Errorf("mos: 模型目录里 ID 重复：%q", opt.ID)
		}
		byID[opt.ID] = opt
		order = append(order, opt.ID)
	}
	cur := order[0]
	if defaultID != "" {
		if _, ok := byID[defaultID]; !ok {
			return nil, fmt.Errorf("mos: 默认模型 %q 不在目录里", defaultID)
		}
		cur = defaultID
	}
	return &modelCatalog{
		byID:  byID,
		order: order,
		cur:   cur,
		shell: newSwitchableModel(byID[cur].Chat),
	}, nil
}

// set 换当前模型（下一轮生效）。ID 不在目录里 → 报错（不静默无效）。
func (c *modelCatalog) set(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	opt, ok := c.byID[id]
	if !ok {
		return fmt.Errorf("mos: 模型 %q 不在目录里", id)
	}
	c.cur = id
	c.shell.set(opt.Chat)
	return nil
}

// currentID 返回当前模型 ID。
func (c *modelCatalog) currentID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cur
}

// list 返回目录（保序）。
func (c *modelCatalog) list() []ModelOption {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]ModelOption, 0, len(c.order))
	for _, id := range c.order {
		out = append(out, c.byID[id])
	}
	return out
}
