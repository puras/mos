package agent

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
