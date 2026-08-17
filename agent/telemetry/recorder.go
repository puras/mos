package telemetry

import (
	"context"
	"sync"
	"time"
)

// EventRecord 是 span 内的一条事件快照。
type EventRecord struct {
	Name       string
	Attributes map[string]string
	Time       time.Time
}

// SpanRecord 是已结束 span 的不可变快照（参考实现导出，供测试断言/本地调试）。
type SpanRecord struct {
	Name       string
	Attributes map[string]string
	Events     []EventRecord
	Start      time.Time
	End        time.Time
	Err        error
	Parent     *SpanRecord
	Children   []*SpanRecord
}

// Recorder 是 TelemetryContext 的参考实现：内存记录 span（含父子树、属性、事件、错误），
// 供本地调试与测试断言，也可作为自研适配器（OpenTelemetry 等）的对照实现。
// 并发安全；span 记录在 End 时落库（未结束的 span 不对外可见）。
type Recorder struct {
	mu    sync.Mutex
	roots []*SpanRecord
}

// NewRecorder 创建空记录器。
func NewRecorder() *Recorder {
	return &Recorder{}
}

// StartSpan 实现 TelemetryContext：从 ctx 取父 span（若有）挂载子记录。
func (r *Recorder) StartSpan(ctx context.Context, name string, attrs map[string]string) (TelemetrySpan, context.Context) {
	rec := &SpanRecord{
		Name:       name,
		Attributes: cloneAttrs(attrs),
		Start:      time.Now(),
	}
	parent, _ := ctx.Value(spanKey{}).(*SpanRecord)
	if parent != nil {
		rec.Parent = parent
	}
	s := &recorderSpan{rec: r, record: rec}
	return s, context.WithValue(ctx, spanKey{}, rec)
}

// Spans 返回全部根 span 快照（含 Children 子树）。
func (r *Recorder) Spans() []SpanRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]SpanRecord, 0, len(r.roots))
	for _, root := range r.roots {
		out = append(out, *cloneRecord(root))
	}
	return out
}

// recorderSpan 是 Recorder 的进行中 span：变更先写内存记录，End 时落库。
type recorderSpan struct {
	rec    *Recorder
	record *SpanRecord
	ended  bool
}

func (s *recorderSpan) SetAttribute(k, v string) {
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	if s.ended || s.record.Attributes == nil {
		return
	}
	s.record.Attributes[k] = v
}

func (s *recorderSpan) AddEvent(name string, attrs map[string]string) {
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	if s.ended {
		return
	}
	s.record.Events = append(s.record.Events, EventRecord{Name: name, Attributes: cloneAttrs(attrs), Time: time.Now()})
}

func (s *recorderSpan) End(err error) {
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	if s.ended {
		return
	}
	s.ended = true
	s.record.End = time.Now()
	s.record.Err = err
	if s.record.Parent == nil {
		s.rec.roots = append(s.rec.roots, s.record)
		return
	}
	s.record.Parent.Children = append(s.record.Parent.Children, s.record)
}

// spanKey 是 ctx 中父 span 记录的键。
type spanKey struct{}

func cloneAttrs(attrs map[string]string) map[string]string {
	if attrs == nil {
		return nil
	}
	out := make(map[string]string, len(attrs))
	for k, v := range attrs {
		out[k] = v
	}
	return out
}

func cloneRecord(rec *SpanRecord) *SpanRecord {
	out := *rec
	out.Attributes = cloneAttrs(rec.Attributes)
	if rec.Events != nil {
		out.Events = make([]EventRecord, len(rec.Events))
		copy(out.Events, rec.Events)
		for i := range out.Events {
			out.Events[i].Attributes = cloneAttrs(rec.Events[i].Attributes)
		}
	}
	if rec.Children != nil {
		out.Children = make([]*SpanRecord, 0, len(rec.Children))
		for _, c := range rec.Children {
			cc := cloneRecord(c)
			cc.Parent = &out
			out.Children = append(out.Children, cc)
		}
	}
	return &out
}
