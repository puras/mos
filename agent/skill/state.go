package skill

import (
	"context"
	"sync"
)

// State 是本次 Run 的技能激活状态（本次任务里加载过哪些技能）。
//
// 挂在 ctx 上（装配层每轮新建一份）：**指针语义**——工具改的是同一个
// State，宿主若自己注入了一份，Run 结束后也能读到（框架不替宿主保管运行期
// 状态，但也不拦着宿主持有它）。
//
// 并发安全：工具可能并发执行（同一批多个 skill 调用）。
type State struct {
	mu        sync.Mutex
	activated []string
	seen      map[string]bool
}

// NewState 造一份空状态。
func NewState() *State {
	return &State{seen: map[string]bool{}}
}

// Activate 记一次激活，返回是否**首次**激活（重复加载不算新激活）。
func (s *State) Activate(name string) bool {
	if s == nil || name == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = map[string]bool{}
	}
	if s.seen[name] {
		return false
	}
	s.seen[name] = true
	s.activated = append(s.activated, name)
	return true
}

// Activated 返回已激活的技能名（按激活顺序）。
func (s *State) Activated() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.activated))
	copy(out, s.activated)
	return out
}

// stateKey 是 ctx 里技能状态的键（私有类型，避免与别的包撞键）。
type stateKey struct{}

// WithState 把技能状态挂进 ctx。
func WithState(ctx context.Context, st *State) context.Context {
	return context.WithValue(ctx, stateKey{}, st)
}

// StateFrom 取 ctx 里的技能状态（没有 = nil）。
func StateFrom(ctx context.Context) *State {
	if ctx == nil {
		return nil
	}
	st, _ := ctx.Value(stateKey{}).(*State)
	return st
}

// Recorder 记技能事件（装配层接会话；nil = 不记）。
//
// 为什么要它：技能正文会改变模型的行为，事后得能查「这轮加载了哪个技能」。
// 框架知道会话，装配层把两者接起来；本包不直接依赖 session（事件类型由
// session 定，载荷由装配层翻译）。
type Recorder interface {
	// RecordActivate 记一条「技能已激活」事件（只记元数据，不记正文）。
	RecordActivate(name, description, dir string) error
}

// recorderKey 是 ctx 里 Recorder 的键。
type recorderKey struct{}

// WithRecorder 把事件记录器挂进 ctx。
func WithRecorder(ctx context.Context, r Recorder) context.Context {
	return context.WithValue(ctx, recorderKey{}, r)
}

// RecorderFrom 取 ctx 里的事件记录器（没有 = nil = 不记）。
func RecorderFrom(ctx context.Context) Recorder {
	if ctx == nil {
		return nil
	}
	r, _ := ctx.Value(recorderKey{}).(Recorder)
	return r
}
