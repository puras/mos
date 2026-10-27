package mos

import (
	"fmt"
	"os"

	"github.com/puras/mos/agent"
)

// 装配层的默认值集中在这里：改默认只改一处，Describe() 与测试都从这里取。
const (
	// DefaultConcurrency 是同一批工具调用的并发上限默认值（4）。
	//
	// 并发是显式 opt-in：工具未声明本次调用可并发时一律独占，所以默认值
	// 仍等价于"多数场景串行"。1 = 串行。
	DefaultConcurrency = 4

	// DefaultTruncateMaxBytes 是单次工具结果的默认截断上限。
	//
	// 与 agent.DefaultTruncateMaxBytes 同值（32KB ≈ 8K token，一次结果占
	// 128K 窗口可用预算 ~7%）。截断只是瘦身，全文落盘可回读。
	DefaultTruncateMaxBytes = agent.DefaultTruncateMaxBytes

	// DefaultSpillDirName 是截断全文的落盘目录名（在工作区内）。
	//
	// 落在工作区内是刻意的：逃生通道（回读全文）必须在模型能读到的根里，
	// 否则"截断了但拿不到全文"就是信息损失。
	DefaultSpillDirName = ".mos-trunc"
)

// applyDefaults 把 Config 的零值字段补成框架默认（就地修改入参副本）。
func (c *Config) applyDefaults() error {
	if c.WorkDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("mos: 取工作目录: %w", err)
		}
		c.WorkDir = wd
	}
	if c.Concurrency <= 0 {
		c.Concurrency = DefaultConcurrency
	}
	if c.ContextWindow <= 0 {
		c.ContextWindow = agent.DefaultContextWindow
	}
	if c.ReserveTokens <= 0 {
		c.ReserveTokens = agent.DefaultReserveTokens
	}
	if c.KeepRecentTokens <= 0 {
		c.KeepRecentTokens = agent.DefaultKeepRecentTokens
	}
	// 截断上限：0 = 默认上限；**负数 = 不截断**（宿主显式说"别截"）。
	if c.TruncateMaxBytes == 0 {
		c.TruncateMaxBytes = DefaultTruncateMaxBytes
	}
	return nil
}
