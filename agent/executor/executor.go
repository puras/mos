// Package executor 提供有界并发批量执行原语（架构 v2 §3.9 并发执行语义）：
// 三阶段模型（preflight 串行决策 → 有界并发执行 → 按输入顺序回填）。
// 语义中立：不感知工具/钩子/日志，由调用方闭包注入，保持零上层依赖。
package executor

import (
	"context"
	"sync"
)

// DefaultMaxParallel 是默认并发上限。
const DefaultMaxParallel = 4

// Item 是批量执行的一个条目；Sequential=true 强制整批回退串行
// （对齐 PI per-tool executionMode="sequential"）。
type Item[T any] struct {
	Input      T
	Sequential bool
}

// Preflight 是串行预检（prepare，架构 v2 §3.9 阶段 1）：对每个条目逐一定夺是否放行。
// 返回 allow=false 表示拦截：条目不进执行阶段，以 Denied 结果直接回填（显式失败回填）。
type Preflight[T any] func(ctx context.Context, input T) (allow bool, err error)

// Execute 是执行阶段（execute，阶段 2）：仅对 preflight 放行的条目调用。
type Execute[T any, R any] func(ctx context.Context, input T) (R, error)

// Outcome 是单个条目的最终结果，按输入顺序回填（与输入等长同序）。
type Outcome[T any, R any] struct {
	Input  T
	Output R
	Err    error
	Denied bool // preflight 拦截：未执行，显式失败回填
}

// Run 执行一批条目（三阶段，对齐 PI executeToolCallsParallel）：
//  1. preflight：按输入顺序串行决策，拦截条目直接进入结果集；
//  2. execute：放行集合有界并发执行；任一条目 Sequential=true（或 maxParallel<=1）
//     时整批按输入顺序串行回退；
//  3. finalize：结果按输入顺序回填（[]Outcome 与输入等长同序）。
//
// ctx 取消传播：尚未开始（preflight 未执行）的条目直接回填 ctx.Err()；
// 已启动的执行随 ctx 取消终止。单条执行失败不影响其他条目（错误进各自 Outcome）。
//
// 注：Go 方法不支持类型参数，故为包级泛型函数（对应设计 §3.9 ConcurrentExecutor.RunBatch）。
func Run[T any, R any](ctx context.Context, items []Item[T], maxParallel int, preflight Preflight[T], execute Execute[T, R]) []Outcome[T, R] {
	if len(items) == 0 {
		return nil
	}
	if maxParallel <= 0 {
		maxParallel = DefaultMaxParallel
	}
	sequential := maxParallel <= 1
	for i := range items {
		if items[i].Sequential {
			sequential = true
			break
		}
	}

	type entry struct {
		output R
		err    error
		denied bool
	}
	entries := make([]entry, len(items))
	allowed := make([]int, 0, len(items))

	// 阶段 1：preflight（串行决策 + 拦截集合）
	for i, it := range items {
		if err := ctx.Err(); err != nil {
			entries[i].err = err
			continue
		}
		allow, err := preflight(ctx, it.Input)
		if !allow || err != nil {
			entries[i].err = err
			entries[i].denied = true
			continue
		}
		allowed = append(allowed, i)
	}

	// 阶段 2：execute（并发 / sequential 回退串行）
	if sequential {
		for _, i := range allowed {
			entries[i].output, entries[i].err = execute(ctx, items[i].Input)
		}
	} else {
		sem := make(chan struct{}, maxParallel)
		var wg sync.WaitGroup
		for _, i := range allowed {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				select {
				case sem <- struct{}{}:
					defer func() { <-sem }()
				case <-ctx.Done():
					entries[idx].err = ctx.Err()
					return
				}
				entries[idx].output, entries[idx].err = execute(ctx, items[idx].Input)
			}(i)
		}
		wg.Wait()
	}

	// 阶段 3：finalize（按输入顺序回填）
	outs := make([]Outcome[T, R], len(items))
	for i, it := range items {
		outs[i] = Outcome[T, R]{Input: it.Input, Output: entries[i].output, Err: entries[i].err, Denied: entries[i].denied}
	}
	return outs
}
