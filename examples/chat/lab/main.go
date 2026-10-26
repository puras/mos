// Package main 是 chat 示例的练手项目：故意留了一个逻辑 bug，
// 让 agent 用 shell 跑测试 → 看到真实失败 → 修好 → 再跑通过。
//
// 这就是"验证回路"：模型不靠猜，靠跑。
package main

import "fmt"

// SumRange 求和 [lo, hi]（**含两端**）。
func SumRange(lo, hi int) int {
	sum := 0
	for i := lo; i <= hi; i++ { // 含两端
		sum += i
	}
	return sum
}

func main() {
	fmt.Println(SumRange(1, 10))
}
