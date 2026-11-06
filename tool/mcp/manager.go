package mcp

import (
	"context"
	"fmt"
	"sync"

	"github.com/puras/mos/tool"
)

// Manager 管多个 MCP 服务器：连接、发现、换代、失败隔离。
//
// 失败隔离（PI / DSH 共识）：一台服务器连不上 → 它不注册工具、快照记一条
// 警告，**其余照常**（FR-D8 的验收口径）。一个服务器崩了不能拖垮整场会话。
//
// 换代两阶段（DSH 语义）：先取**全量**新工具，再整体换进注册表；换代失败
// 保留上一代（不留半套工具）。注册表的 Replace 本身是原子的，所以中间态
// 模型看不到。
type Manager struct {
	opts Options
	reg  *tool.Registry

	mu       sync.Mutex
	servers  []*Server
	warnings []string
	skipped  []string

	hidden *hiddenSet
	// resRegistered：资源工具是否已注册（RegisterResourceTools 幂等守卫）。
	resRegistered bool

	refreshCh  chan string
	closeCh    chan struct{}
	closeOnce  sync.Once
	loopDone   chan struct{}
	refreshCtx context.Context
}

// Server 是一个 MCP 服务器的运行时状态。
type Server struct {
	cfg    ServerConfig
	client Client

	mu           sync.RWMutex
	tools        []*Tool
	instructions string
	protocol     string
	// hasRes：连接后是否声明了资源/模板（startServer 探测一次）。
	hasRes bool
}

// NewManager 造一个 MCP 编排器（不连接——连接发生在 Start）。
func NewManager(opts Options, reg *tool.Registry) *Manager {
	if opts.ConnectTimeout <= 0 {
		opts.ConnectTimeout = DefaultConnectTimeout
	}
	if opts.CallTimeout <= 0 {
		opts.CallTimeout = DefaultCallTimeout
	}
	m := &Manager{
		opts:      opts,
		reg:       reg,
		hidden:    newHiddenSet(),
		refreshCh: make(chan string, 8),
		closeCh:   make(chan struct{}),
		loopDone:  make(chan struct{}),
	}
	// 换代信号回调：只投递信号，不在 SDK 的读循环里干活。
	//
	// 宿主的回调**同时**保留：内部信号（换代）与宿主回调（通知宿主）
	// 两件事互不替代——宿主要的是「服务器工具变了」这件事，内部要的是
	// 「去换代」。任一方缺席都不对。
	userChanged := opts.OnToolsChanged
	opts.OnToolsChanged = func(server string) {
		select {
		case m.refreshCh <- server:
		default: // 信号已满：丢弃（下一轮换代会覆盖；不阻塞 SDK 读循环）
		}
		if userChanged != nil {
			userChanged(server)
		}
	}
	m.opts.OnToolsChanged = opts.OnToolsChanged
	return m
}

// Start 连接全部服务器并注册它们的工具。
//
// **并发连接**（PI 同款，`extensions/mcp/index.ts:1141` 对 enabled 逐台
// `startConnection` 后 `Promise.all` 等全量；DSH 的 mcp-client 走插件框架、
// 每插件独立连接——两家的单台连接都是独立失败单元）：N 台慢服务器不再让
// 装配时间线性叠加，单台最坏只吃满自己的 ConnectTimeout。返回前所有连接
// 已就绪（每台一个 goroutine + WaitGroup 等全量），因此 Start 返回后即可
// RegisterResourceTools / 查快照。某台失败只记警告，不影响其余（失败隔离）。
// 返回的 ready 在 WaitGroup.Wait 完成后关闭——宿主想在装配后台干别的活
// 时用它等就绪，同步装配的宿主可忽略（Start 返回时已关闭）。
func (m *Manager) Start(ctx context.Context) (ready <-chan struct{}) {
	var wg sync.WaitGroup
	for _, cfg := range m.opts.Servers {
		if cfg.Disabled {
			// 故意不连（配置留着不启用）——快照里要写明「没装」，别让宿主
			// 以为是自己写错了名字。
			m.skip(cfg.Name)
			continue
		}
		wg.Add(1)
		go func(cfg ServerConfig) {
			defer wg.Done()
			if err := m.startServer(ctx, cfg); err != nil {
				// 认证失败单独说：处置不同——重试没用，得先把凭据给对。
				if IsAuthError(err) {
					m.warn("MCP 服务器 %q 需要认证（不可用，请检查 token 或 TokenProvider）: %v", cfg.Name, err)
				} else {
					m.warn("MCP 服务器 %q 不可用: %v", cfg.Name, err)
				}
			}
		}(cfg)
	}
	done := make(chan struct{})
	ready = done
	go func() {
		wg.Wait()
		close(done)
		go m.refreshLoop()
	}()
	wg.Wait()
	return ready
}

// RegisterResourceTools 注册三个共享资源工具（mcp_resource_list / templates
// / read，S8.4-E）。装配层在 Start 之后调。
//
// 没有任何一台可见服务器有资源时不注册（工具面恒定为 0——模型不会拿到
// 一组永远「未知服务器」的工具）；已注册过则幂等返回。
// 只要有一台可达就注册全部三个：工具面是共享的（显式 server 参数寻址），
// 不随有资源的服务器数变化。
func (m *Manager) RegisterResourceTools() error {
	m.mu.Lock()
	if m.resRegistered {
		m.mu.Unlock()
		return nil
	}
	for _, s := range m.servers {
		if s.hasResources() && !m.serverHidden(s.cfg) {
			// 先占坑再注册：并发调用下第二个调用者看到 resRegistered=true
			// 直接返回，不会重名报错（注册失败才回滚标记）。
			m.resRegistered = true
			m.mu.Unlock()
			if err := m.registerResourceTools(); err != nil {
				m.mu.Lock()
				m.resRegistered = false
				m.mu.Unlock()
				return err
			}
			return nil
		}
	}
	m.mu.Unlock()
	return nil
}

// startServer 连接一台服务器并注册它的工具。
func (m *Manager) startServer(ctx context.Context, cfg ServerConfig) error {
	dial := m.opts.Dial
	if dial == nil {
		dial = Dial
	}
	client, err := dial(ctx, cfg, m.opts)
	if err != nil {
		return err
	}
	infos, err := client.ListTools(ctx)
	if err != nil {
		_ = client.Close()
		return err
	}
	s := &Server{cfg: cfg, client: client}
	if err := m.register(s, infos); err != nil {
		_ = client.Close()
		return err
	}
	// 资源能力探测：失败按没有资源处理（不把偶发抖动拦在资源工具之外）。
	// 探测失败不再另外记警告——一台没有资源的服务器是常态，不是故障。
	hasRes := false
	resCtx, cancel := context.WithTimeout(ctx, m.opts.CallTimeout)
	if _, err := client.ListResources(resCtx); err == nil {
		hasRes = true
	} else if _, err := client.ListResourceTemplates(resCtx); err == nil {
		hasRes = true
	}
	cancel()
	s.mu.Lock()
	s.instructions = client.Instructions()
	s.protocol = client.ProtocolVersion()
	s.hasRes = hasRes
	s.mu.Unlock()
	m.mu.Lock()
	m.servers = append(m.servers, s)
	m.mu.Unlock()
	return nil
}

// register 把一个服务器的工具集换进注册表（原子）。
//
// exposure 决定三档：
//   - deferred：注册成 deferred 工具（不进模型清单，搜到才进）；
//   - direct：直接进模型清单；
//   - hidden：进注册表但被 hidden 可见性层挡住（模型看不见、也搜不到）。
func (m *Manager) register(s *Server, infos []ToolInfo) error {
	tools := make([]*Tool, 0, len(infos))
	regTools := make([]tool.Tool, 0, len(infos))
	deferred := make(map[string]bool, len(infos))
	hidden := make([]string, 0, len(infos))
	for _, info := range infos {
		t := NewTool(s.cfg.Name, info, s.client)
		tools = append(tools, t)
		regTools = append(regTools, t)
		switch m.exposure(s.cfg, info.Name) {
		case ExposureDirect:
			deferred[t.Name()] = false
		case ExposureHidden:
			deferred[t.Name()] = false
			hidden = append(hidden, t.Name())
		default:
			deferred[t.Name()] = true
		}
	}
	if err := m.reg.Replace(scopeOf(s.cfg.Name), regTools, deferred); err != nil {
		return err
	}
	m.hidden.set(s.cfg.Name, hidden)
	s.mu.Lock()
	s.tools = tools
	s.mu.Unlock()
	return nil
}

// exposure 解析某个工具的暴露档：精确名优先于 "*"（PI 同款语义）。
func (m *Manager) exposure(cfg ServerConfig, rawTool string) Exposure {
	if e, ok := cfg.ToolExposure[rawTool]; ok {
		return ParseExposure(string(e))
	}
	if e, ok := cfg.ToolExposure["*"]; ok {
		return ParseExposure(string(e))
	}
	if cfg.Exposure != "" {
		return ParseExposure(string(cfg.Exposure))
	}
	return ExposureDeferred
}

// refreshLoop 处理换代信号（tools/list_changed）。
//
// 换代在**别的 goroutine** 里做：SDK 的通知回调跑在它的读循环上，
// 在那里发 tools/list 会自锁。
func (m *Manager) refreshLoop() {
	defer close(m.loopDone)
	for {
		select {
		case <-m.closeCh:
			return
		case name := <-m.refreshCh:
			m.refreshServer(name)
		}
	}
}

// refreshServer 重新拉取一个服务器的工具并换代。
//
// 两阶段：先取全量（取失败 → 保留上一代），再换进注册表（换代失败 →
// 保留上一代）。任何一步失败都不留半套工具。
func (m *Manager) refreshServer(name string) {
	s := m.serverByName(name)
	if s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), m.opts.CallTimeout)
	defer cancel()
	infos, err := s.client.ListTools(ctx)
	if err != nil {
		m.warn("MCP 服务器 %q 刷新失败（保留上一代工具）: %v", name, err)
		return
	}
	if err := m.register(s, infos); err != nil {
		m.warn("MCP 服务器 %q 换代失败（保留上一代工具）: %v", name, err)
		return
	}
}

// serverByName 按名字找服务器。
func (m *Manager) serverByName(name string) *Server {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.servers {
		if s.cfg.Name == name {
			return s
		}
	}
	return nil
}

// Servers 返回已连接的服务器。
func (m *Manager) Servers() []*Server {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*Server(nil), m.servers...)
}

// Warnings 返回降级诊断（连接失败的服务器、换代失败……）。
func (m *Manager) Warnings() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.warnings...)
}

// warn 记一条诊断。
func (m *Manager) warn(format string, args ...any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.warnings = append(m.warnings, fmt.Sprintf(format, args...))
}

// skip 记下一个**故意没连**的服务器（配置为禁用）。
func (m *Manager) skip(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.skipped = append(m.skipped, name)
}

// Skipped 返回故意没连的服务器名（配置为禁用）。
//
// 与 Warnings 分开：禁用不是故障，是配置意图；但快照仍要写出来（“没装什么”）。
func (m *Manager) Skipped() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.skipped...)
}

// Instruction 是一条服务器说明（连接建立时一次性注入给模型）。
type Instruction struct {
	// Server 是服务器名。
	Server string
	// Text 是服务器给的说明正文。
	Text string
}

// Instructions 返回各服务器的说明（空的不要）。
func (m *Manager) Instructions() []Instruction {
	var out []Instruction
	for _, s := range m.Servers() {
		s.mu.RLock()
		text := s.instructions
		s.mu.RUnlock()
		if text == "" {
			continue
		}
		out = append(out, Instruction{Server: s.cfg.Name, Text: text})
	}
	return out
}

// HiddenFilter 返回 hidden 档的可见性过滤器（模型看不见 hidden 工具）。
//
// 它读的是**当前**的 hidden 集合——服务器换代时集合会变，过滤器本身不用换
// （过滤器必须是纯判定、无副作用）。
func (m *Manager) HiddenFilter() func(name string) bool {
	return func(name string) bool { return !m.hidden.has(name) }
}

// ToolCount 返回各档工具数（快照用）。
func (m *Manager) ToolCount() (deferred, direct, hidden int) {
	for _, s := range m.Servers() {
		s.mu.RLock()
		tools := append([]*Tool(nil), s.tools...)
		s.mu.RUnlock()
		for _, t := range tools {
			switch m.exposure(s.cfg, t.RawName()) {
			case ExposureDirect:
				direct++
			case ExposureHidden:
				hidden++
			default:
				deferred++
			}
		}
	}
	return deferred, direct, hidden
}

// Close 关闭全部连接并停止换代循环。
func (m *Manager) Close() error {
	m.closeOnce.Do(func() {
		close(m.closeCh)
		<-m.loopDone
	})
	var firstErr error
	for _, s := range m.Servers() {
		if err := s.client.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Name 返回服务器名。
func (s *Server) Name() string { return s.cfg.Name }

// Description 返回服务器描述。
func (s *Server) Description() string { return s.cfg.Description }

// ProtocolVersion 返回协商后的协议版本。
func (s *Server) ProtocolVersion() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.protocol
}

// Instructions 返回服务器说明。
func (s *Server) Instructions() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.instructions
}

// Tools 返回当前代的工具。
func (s *Server) Tools() []*Tool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]*Tool(nil), s.tools...)
}

// HasResources 报告这台服务器连接后是否声明了资源或模板。
//
// startServer 里探测一次并缓存：资源列表会变，但「有没有资源能力」在
// 会话生命周期里当作不变处理（与 instructions 同口径——探测失败按没有
// 资源处理，不把一台偶发抖动的服务器整个拦在资源工具之外）。
func (s *Server) HasResources() bool { return s.hasResources() }

func (s *Server) hasResources() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.hasRes
}

// scopeOf 造一个服务器的代际标识（注册表换代按它隔离）。
func scopeOf(server string) string { return "mcp:" + server }

// hiddenSet 记录当前哪些工具处于 hidden 档（按服务器分组，便于换代时整组替换）。
type hiddenSet struct {
	mu       sync.RWMutex
	byServer map[string][]string
	names    map[string]bool
}

func newHiddenSet() *hiddenSet {
	return &hiddenSet{byServer: map[string][]string{}, names: map[string]bool{}}
}

// set 替换某个服务器的 hidden 名单。
func (h *hiddenSet) set(server string, names []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, old := range h.byServer[server] {
		delete(h.names, old)
	}
	h.byServer[server] = append([]string(nil), names...)
	for _, n := range names {
		h.names[n] = true
	}
}

// has 判断工具是否处于 hidden 档。
func (h *hiddenSet) has(name string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.names[name]
}
