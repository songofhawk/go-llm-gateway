# 独立课程拆分验收

2026-10-07，在 Go 1.27.1 / darwin arm64 环境检查。

## 代码边界

每课有自己的 go.mod、源码和 Mock；第 2 课起有本课配置，第 4 课起有 SQLite 依赖及 go.sum。各课没有 replace、go.work 或对根目录/其他课程的代码引用。

| 课程 | 顶层测试数 | 本课边界 |
| --- | ---: | --- |
| 01-proxy | 7 | 标准库 ReverseProxy，没有路由模块 |
| 02-routing | 21 | 轮转、映射、fallback；没有容量门、SQLite、pprof、h2c、令牌桶 |
| 03-concurrency | 25 | 增加容量、按负载选择、读写时限；没有后台任务 |
| 04-jobs | 37 | 增加 SQLite 和 worker；没有诊断与速率桶 |
| 05-labs | 37 | 业务源码与第 4 课一致，增加 scripts/labs.sh |
| 06-pprof | 48 | 增加 pprof、loadtest、stress.sh、可选 h2c；没有速率桶 |
| 07-rate-limiting | 57 | 增加令牌桶、入口 429 和 rate/burst 参数 |

测试数包含各课自己的 Mock 测试，不含状态码等子用例；相同测试会在不同独立版本中重复存在。

## 已执行的检查

- 七课的普通测试全部通过：`bash scripts/verify-lessons.sh`。
- 七课的竞态检测全部通过：`bash scripts/verify-lessons.sh race`。
- 根目录完整参考实现的 `go test -timeout 60s ./...` 通过。
- 分别构建并启动七课自己的 Mock 和网关，使用临时配置/端口验证 JSON 和完整 SSE；第 4～7 课验证任务提交、查询至 done；第 6～7 课验证独立诊断端口。
- 第 5 课 `bash scripts/labs.sh all` 全部实验通过，包括取消、容量、超时、截断、慢客户端和恢复。
- 第 6 课实际运行 1 秒 unary 负载和 pprof 采样，脚本成功结束，生成结果 JSON、CPU/heap/goroutine/block/mutex 原始采样与文本分析。
- 检查课程和教程中的新增本地 Markdown 链接，目标存在；脚本通过 bash 语法检查；课程 Go 源码符合 gofmt。

## 运行方式变化

课程是嵌套 Go 模块，根目录的 `go test ./...` 不会进入课程目录。课程测试使用汇总脚本或先 cd 进入对应课程。第 1 课也已改为独立模块：进入 `lessons/01-proxy` 后执行 `go run .`；各课 README 和原教程的命令已更新。

这里记录的是课程拆分的可运行性和行为检查。完整项目的历史压测结果仍见根目录 docs/STRESS-REPORT.md，本次短 unary 检查没有重做历史所有负载场景。
