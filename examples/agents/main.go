package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

// ============================================================================
// 任务管理
// ============================================================================

type TaskStatus string

const (
	StatusPending   TaskStatus = "pending"
	StatusPlanning  TaskStatus = "planning"
	StatusExecuting TaskStatus = "executing"
	StatusReviewing TaskStatus = "reviewing"
	StatusCompleted TaskStatus = "completed"
	StatusRejected  TaskStatus = "rejected"
)

type Task struct {
	ID          int
	Description string
	Status      TaskStatus
	Subtasks    []string
	Result      string
	Feedback    string
	CreatedAt   time.Time
}

// ============================================================================
// Agent 接口定义
// ============================================================================

type Agent interface {
	GetName() string
	GetType() string
}

// PlannerAgent 拆解任务
type PlannerAgent interface {
	Agent
	Process(ctx context.Context, input string) ([]string, error)
}

// WorkerAgent 执行任务
type WorkerAgent interface {
	Agent
	Process(ctx context.Context, subtasks []string) (string, error)
}

// ReviewerAgent 验收任务
type ReviewerAgent interface {
	Agent
	Process(ctx context.Context, result string) (string, error)
}

// ============================================================================
// Planner Agent - 任务拆解
// ============================================================================

type DefaultPlanner struct {
	AgentName string
}

func NewDefaultPlanner() *DefaultPlanner {
	return &DefaultPlanner{AgentName: "拆解专家"}
}

func (a *DefaultPlanner) GetName() string { return a.AgentName }
func (a *DefaultPlanner) GetType() string { return "planner" }

func (a *DefaultPlanner) Process(ctx context.Context, input string) ([]string, error) {
	subtasks := []string{}

	inputLower := strings.ToLower(input)

	if strings.Contains(inputLower, "文档") || strings.Contains(inputLower, "写") {
		subtasks = []string{
			"📋 分析需求：理解文档主题和目标读者",
			"📝 构建大纲：组织内容结构",
			"✍️ 撰写初稿：按照大纲填充内容",
			"🔍 审阅修订：检查语法和逻辑",
		}
	} else if strings.Contains(inputLower, "代码") || strings.Contains(inputLower, "实现") || strings.Contains(inputLower, "功能") {
		subtasks = []string{
			"🔧 需求分析：明确功能边界",
			"🎨 架构设计：选择技术方案",
			"💻 编写代码：实现核心逻辑",
			"🧪 单元测试：验证功能正确性",
			"🔒 安全检查：审查潜在风险",
		}
	} else if strings.Contains(inputLower, "研究") || strings.Contains(inputLower, "调研") {
		subtasks = []string{
			"📚 信息收集：搜索相关资料",
			"📊 数据分析：整理关键信息",
			"📝 撰写报告：输出研究结论",
		}
	} else {
		subtasks = []string{
			"📌 理解任务目标",
			"📋 制定执行计划",
			"⚡ 执行各个步骤",
			"✅ 验证完成标准",
		}
	}

	time.Sleep(500 * time.Millisecond)
	return subtasks, nil
}

// ============================================================================
// Worker Agent - 任务执行
// ============================================================================

type DefaultWorker struct {
	AgentName string
}

func NewDefaultWorker() *DefaultWorker {
	return &DefaultWorker{AgentName: "执行专家"}
}

func (a *DefaultWorker) GetName() string { return a.AgentName }
func (a *DefaultWorker) GetType() string { return "worker" }

func (a *DefaultWorker) Process(ctx context.Context, subtasks []string) (string, error) {
	var result strings.Builder
	result.WriteString("=== 执行报告 ===\n\n")

	for i, task := range subtasks {
		fmt.Printf("   ⏳ 正在执行: %s\n", task)
		time.Sleep(300 * time.Millisecond)
		result.WriteString(fmt.Sprintf("[%d] ✅ %s - 已完成\n", i+1, task))
	}

	result.WriteString("\n🎉 所有子任务执行完毕！")
	time.Sleep(500 * time.Millisecond)
	return result.String(), nil
}

// ============================================================================
// Reviewer Agent - 任务验收
// ============================================================================

type DefaultReviewer struct {
	AgentName string
}

func NewDefaultReviewer() *DefaultReviewer {
	return &DefaultReviewer{AgentName: "验收专家"}
}

func (a *DefaultReviewer) GetName() string { return a.AgentName }
func (a *DefaultReviewer) GetType() string { return "reviewer" }

func (a *DefaultReviewer) Process(ctx context.Context, result string) (string, error) {
	fmt.Println("   🔍 正在检查执行结果...")
	time.Sleep(800 * time.Millisecond)

	hasCompletion := strings.Contains(result, "执行完毕") || strings.Contains(result, "✅")

	var feedback string
	if hasCompletion {
		feedback = "✅ 验收通过：任务已完整执行，所有步骤均已完成"
	} else {
		feedback = "❌ 验收不通过：执行结果不完整，需要重新执行"
	}

	fmt.Printf("   📋 %s\n", feedback)
	time.Sleep(300 * time.Millisecond)
	return feedback, nil
}

// ============================================================================
// 任务流水线
// ============================================================================

type Pipeline struct {
	planner  PlannerAgent
	worker   WorkerAgent
	reviewer ReviewerAgent
	tasks    map[int]*Task
	nextID   int
}

func NewPipeline() *Pipeline {
	return &Pipeline{
		planner:  NewDefaultPlanner(),
		worker:   NewDefaultWorker(),
		reviewer: NewDefaultReviewer(),
		tasks:    make(map[int]*Task),
		nextID:   1,
	}
}

func (p *Pipeline) SubmitTask(description string) *Task {
	task := &Task{
		ID:          p.nextID,
		Description: description,
		Status:      StatusPending,
		CreatedAt:   time.Now(),
	}
	p.tasks[task.ID] = task
	p.nextID++
	return task
}

func (p *Pipeline) ProcessTask(ctx context.Context, taskID int) error {
	task, ok := p.tasks[taskID]
	if !ok {
		return fmt.Errorf("任务 #%d 不存在", taskID)
	}

	fmt.Printf("\n🚀 开始处理任务 #%d\n", taskID)
	fmt.Printf("   描述: %s\n", task.Description)
	fmt.Println(strings.Repeat("-", 50))

	// Phase 1: 拆解
	task.Status = StatusPlanning
	fmt.Printf("\n📋 [阶段1] 任务拆解 - 负责人: %s\n", p.planner.GetName())
	fmt.Println(strings.Repeat("-", 40))

	subtasks, err := p.planner.Process(ctx, task.Description)
	if err != nil {
		return fmt.Errorf("拆解失败: %w", err)
	}

	task.Subtasks = subtasks

	fmt.Printf("\n✅ 拆解完成，共 %d 个子任务:\n", len(subtasks))
	for i, t := range subtasks {
		fmt.Printf("   %d. %s\n", i+1, t)
	}

	// Phase 2: 执行
	task.Status = StatusExecuting
	fmt.Printf("\n⚡ [阶段2] 任务执行 - 负责人: %s\n", p.worker.GetName())
	fmt.Println(strings.Repeat("-", 40))

	result, err := p.worker.Process(ctx, subtasks)
	if err != nil {
		return fmt.Errorf("执行失败: %w", err)
	}

	task.Result = result

	// Phase 3: 验收
	task.Status = StatusReviewing
	fmt.Printf("\n🔍 [阶段3] 任务验收 - 负责人: %s\n", p.reviewer.GetName())
	fmt.Println(strings.Repeat("-", 40))

	feedback, err := p.reviewer.Process(ctx, result)
	if err != nil {
		return fmt.Errorf("验收失败: %w", err)
	}

	task.Feedback = feedback

	// 更新最终状态
	if strings.Contains(feedback, "通过") {
		task.Status = StatusCompleted
		fmt.Printf("\n🎉 任务 #%d 已完成！\n", taskID)
	} else {
		task.Status = StatusRejected
		fmt.Printf("\n❌ 任务 #%d 未通过验收\n", taskID)
	}

	return nil
}

func (p *Pipeline) ListTasks() {
	fmt.Println("\n📊 当前任务列表")
	fmt.Println(strings.Repeat("=", 60))

	if len(p.tasks) == 0 {
		fmt.Println("   暂无任务")
		return
	}

	for _, task := range p.tasks {
		statusIcon := "⏳"
		switch task.Status {
		case StatusPlanning:
			statusIcon = "📋"
		case StatusExecuting:
			statusIcon = "⚡"
		case StatusReviewing:
			statusIcon = "🔍"
		case StatusCompleted:
			statusIcon = "✅"
		case StatusRejected:
			statusIcon = "❌"
		}

		fmt.Printf("\n%s 任务 #%d - %s\n", statusIcon, task.ID, task.Description)
		fmt.Printf("   状态: %s | 子任务: %d 个\n", task.Status, len(task.Subtasks))
		fmt.Printf("   创建时间: %s\n", task.CreatedAt.Format("15:04:05"))

		if task.Feedback != "" {
			fmt.Printf("   验收结果: %s\n", task.Feedback)
		}
	}
	fmt.Println(strings.Repeat("=", 60))
}

// ============================================================================
// 交互式 CLI
// ============================================================================

type CLI struct {
	pipeline *Pipeline
	scanner  *bufio.Scanner
}

func NewCLI() *CLI {
	return &CLI{
		pipeline: NewPipeline(),
		scanner:  bufio.NewScanner(os.Stdin),
	}
}

func (c *CLI) Run() {
	fmt.Println("╔══════════════════════════════════════════════════════════╗")
	fmt.Println("║           🤖 多 Agent 协作系统                              ║")
	fmt.Println("╠══════════════════════════════════════════════════════════╣")
	fmt.Println("║  三个专家协同工作：拆解 → 执行 → 验收                      ║")
	fmt.Println("╚══════════════════════════════════════════════════════════╝")
	fmt.Println()

	c.printHelp()

	for {
		fmt.Print("\n➜ 请输入命令: ")
		if !c.scanner.Scan() {
			break
		}

		input := strings.TrimSpace(c.scanner.Text())
		if input == "" {
			continue
		}

		c.handleCommand(input)
	}
}

func (c *CLI) printHelp() {
	fmt.Println("\n可用命令:")
	fmt.Println("  new <任务描述>    - 创建新任务")
	fmt.Println("  run <任务ID>      - 执行指定任务")
	fmt.Println("  list              - 查看所有任务")
	fmt.Println("  clear             - 清空所有任务")
	fmt.Println("  help              - 显示帮助")
	fmt.Println("  quit / exit       - 退出程序")
	fmt.Println("\n示例:")
	fmt.Println("  new 写一篇关于AI的文章")
	fmt.Println("  new 实现一个用户登录功能")
	fmt.Println("  run 1")
	fmt.Println("  list")
}

func (c *CLI) handleCommand(input string) {
	parts := strings.SplitN(input, " ", 2)
	command := strings.ToLower(parts[0])
	args := ""
	if len(parts) > 1 {
		args = strings.TrimSpace(parts[1])
	}

	switch command {
	case "new":
		if args == "" {
			fmt.Println("❌ 请提供任务描述，例如: new 写一篇关于AI的文章")
			return
		}
		task := c.pipeline.SubmitTask(args)
		fmt.Printf("✅ 任务已创建: #%d - %s\n", task.ID, task.Description)

	case "run":
		if args == "" {
			fmt.Println("❌ 请提供任务ID，例如: run 1")
			return
		}

		var taskID int
		fmt.Sscanf(args, "%d", &taskID)

		ctx := context.Background()
		if err := c.pipeline.ProcessTask(ctx, taskID); err != nil {
			fmt.Printf("❌ 执行失败: %v\n", err)
		}

	case "list":
		c.pipeline.ListTasks()

	case "clear":
		c.pipeline = NewPipeline()
		fmt.Println("✅ 所有任务已清空")

	case "help":
		c.printHelp()

	case "quit", "exit":
		fmt.Println("👋 再见！")
		os.Exit(0)

	default:
		fmt.Printf("❌ 未知命令: %s (输入 help 查看帮助)\n", command)
	}
}

// ============================================================================
// 主函数
// ============================================================================

func main() {
	cli := NewCLI()
	cli.Run()
}
