package agent

import (
	"encoding/json"
	"fmt"
)

// Session 表示一次对话会话，持有会话级的内部消息历史与可信任务状态。
// History 采用两层消息的第一层（InternalMessage，M2.4）：除 LLM 标准消息外
// 携带运行期元数据（ExcludeFromContext / Summary / Source），发送给模型前
// 经 convertToLlm 翻译边界转为标准消息；system 引导由 Agent 在每次执行时注入。
// State 独立于历史存在：历史可压缩/可刷新，已验证进度始终保留。
type Session struct {
	ID      string
	History []InternalMessage
	State   TaskState
}

// NewSession 创建指定 ID 的空会话。
func NewSession(id string) *Session {
	return &Session{ID: id}
}

// sessionSnapshot 是 Session 的稳定序列化结构（平台持久化契约，
// 见 docs/sdk-contracts.md §3）。字段契约不可随意变更：平台侧可能按此结构落库。
type sessionSnapshot struct {
	ID      string            `json:"id"`
	History []InternalMessage `json:"history,omitempty"`
	State   TaskState         `json:"state"`
}

// Save 将会话（ID + 消息历史 + 任务状态）序列化为 JSON，供平台持久化。
// 注意：只序列化数据；Agent 配置（模型对象 / 工具集 / 策略）由平台自行持有，
// 恢复会话后需重建 Agent 再 Run 续跑。
func (s *Session) Save() ([]byte, error) {
	return json.Marshal(sessionSnapshot{ID: s.ID, History: s.History, State: s.State})
}

// RestoreSession 从 Save 产出的 JSON 恢复会话，用于跨进程 / 重启后的断点续跑。
// 恢复后的会话可直接传给 Agent.Run 继续执行（run 会自动将非 running 状态置为 running）。
func RestoreSession(data []byte) (*Session, error) {
	var snap sessionSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("agent: restore session: %w", err)
	}
	return &Session{ID: snap.ID, History: snap.History, State: snap.State}, nil
}
