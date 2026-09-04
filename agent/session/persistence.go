package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/puras/mos/agent/event"
)

// ErrNotFound 表示会话不存在（无持久化记录）；Restore 按空会话处理，不报错。
var ErrNotFound = errors.New("session not found")

// Persistence 是会话事件日志持久化后端（架构 v2 §10.3，对齐 dsh SessionPersistence）：
// backend 可换——jsonl 由 SDK 内置（纯文件）；sqlite / postgres 等数据库后端由业务层
// 实现本接口并注入（MoS 不操作数据库）。事件日志是唯一事实来源，恢复 = 日志重放。
// Header 与事件日志分离存储（格式版本 / cwd / lineage / seed 不进事件流）。
type Persistence interface {
	// SaveHeader 写入会话头部元数据（首次落盘时调用一次）。
	SaveHeader(sessionID string, h SessionHeader) error
	// LoadHeader 读取会话头部元数据。
	LoadHeader(sessionID string) (SessionHeader, error)
	// Append 追加一条事件（Seq 连续，顺序落盘）。
	Append(sessionID string, ev SessionEvent) error
	// Load 读取会话全部事件（按 Seq 升序）。
	Load(sessionID string) ([]SessionEvent, error)
	// SaveState 持久化任务状态（TaskState：目标/已验证事实/状态机，M2.8）。
	// 崩溃恢复（RepairInterrupted 之外）与挂起-恢复时还原任务进度。实现可复用
	// 会话后端目录，存独立文件（如 <sid>.state.json），原子写。
	SaveState(sessionID string, st TaskState) error
	// LoadState 读取任务状态；无记录返回 ErrNotFound。
	LoadState(sessionID string) (TaskState, error)
	// Close 关闭后端（刷盘/释放资源）。
	Close() error
}

// Restore 从持久化后端重放事件重建会话（架构 v2 §3.2：恢复 = 日志重放）。
// 崩溃恢复语义（§10.3 crash recovery）：重载到孤儿 turn/start（无配对 turn/end）
// 时不截断，追加合成 turn/end{reason: interrupted} 平衡日志。
// 任务状态（TaskState，M2.8）若后端有持久化记录则一并恢复（目标/已验证事实/状态机）；
// 无记录时按空状态重建。恢复后的会话可直接传给 Driver.Run 继续执行；
// 如需继续落盘，设置 Persistence 字段。
func Restore(p Persistence, sessionID string) (*Session, error) {
	if p == nil {
		return nil, fmt.Errorf("agent: restore: nil persistence")
	}
	h, err := p.LoadHeader(sessionID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// 无持久化记录 = 空会话：继续加载事件（文件存在但缺 header 时仍有事件可恢复）。
			h = SessionHeader{}
		} else {
			return nil, fmt.Errorf("agent: restore: load header: %w", err)
		}
	}
	events, err := p.Load(sessionID)
	if err != nil {
		return nil, fmt.Errorf("agent: restore: load events: %w", err)
	}
	events = RepairInterrupted(events)
	if h.Version == 0 {
		h.Version = sessionFormatVersion
	}
	sess := &Session{ID: sessionID, Events: events, Header: h}
	// 任务状态恢复（M2.8）：有持久化记录则覆盖默认空状态；无记录保持空（按 events 重建）。
	if st, err := p.LoadState(sessionID); err == nil {
		sess.State = st
	} else if !errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("agent: restore: load state: %w", err)
	}
	return sess, nil
}

// RepairInterrupted 修复孤儿 turn/start（无配对 turn/end）：追加合成
// turn/end{reason: interrupted} 平衡日志（§10.3 crash recovery，不截断）。
// 按 turn 号集合求差判定孤儿（而非栈配对），可正确处理嵌套/乱序的 turn 号：
// 例：start(1) → start(2) → end(1) 时，孤儿是 turn 2 而非 turn 1。
func RepairInterrupted(events []SessionEvent) []SessionEvent {
	started := make(map[int]bool)
	ended := make(map[int]bool)
	for _, ev := range events {
		switch ev.Type {
		case event.SessionEventTurnStart:
			var d event.TurnData
			_ = json.Unmarshal(ev.Data, &d)
			started[d.Turn] = true
		case event.SessionEventTurnEnd:
			var d event.TurnData
			_ = json.Unmarshal(ev.Data, &d)
			ended[d.Turn] = true
		}
	}
	var orphans []int
	for turn := range started {
		if !ended[turn] {
			orphans = append(orphans, turn)
		}
	}
	sort.Ints(orphans)
	for _, turn := range orphans {
		events = append(events, SessionEvent{
			Seq:  int64(len(events)) + 1,
			Type: event.SessionEventTurnEnd,
			Data: event.MarshalData(event.TurnData{Turn: turn, Reason: "interrupted"}),
		})
	}
	return events
}
