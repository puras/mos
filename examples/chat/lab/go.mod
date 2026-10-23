// 独立的 module：故意与根 module 分开，这样它不会进入仓库的 go build ./... /
// go test ./...（仓库 CI 必须始终是绿的）。示例里用 `cd lab && go test ./...` 跑它。
module moslab

go 1.26
