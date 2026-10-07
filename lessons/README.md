# 按课程逐步运行的代码版本

先进入一课，只读这一课目录内的代码。每一课都有自己的 Go 模块、源码、Mock、README；第 2 课起包含配置，第 4 课起包含 SQLite 依赖。复制任意一课目录出来也能独立运行。

每份版本累计前面学过的功能。课程目录没有导入仓库根目录的 internal，也没有导入其他课程；没有用开关把后续功能藏在早期课程中。

| 文档 | 独立代码 | 本步新增内容 |
| --- | --- | --- |
| [第 1 步：最小代理](../docs/01-proxy.md) | [01-proxy](01-proxy/README.md) | 固定上游、JSON/SSE 转发、请求取消 |
| [第 2 步：模型路由与 fallback](../docs/02-routing.md) | [02-routing](02-routing/README.md) | 逻辑模型档次、Provider 接口、同组轮转、有限 fallback |
| [第 3 步：并发与生命周期](../docs/03-concurrency.md) | [03-concurrency](03-concurrency/README.md) | 入口及供应商容量、按负载选择、读写超时、优雅停机 |
| [第 4 步：持久化后台任务](../docs/04-jobs.md) | [04-jobs](04-jobs/README.md) | SQLite、任务状态机、固定 worker、重启恢复 |
| [第 5 步：故障实验](../docs/05-labs.md) | [05-labs](05-labs/README.md) | 沿用第 4 课功能，新增可执行故障实验与观察清单 |
| [第 6 步：Mock、压测与 pprof](../docs/06-pprof.md) | [06-pprof](06-pprof/README.md) | 独立诊断端口、压测客户端、采样脚本、可选 h2c |
| [第 7 步：请求速率限流](../docs/07-rate-limiting.md) | [07-rate-limiting](07-rate-limiting/README.md) | 令牌桶、rate/burst、入口 429、共享额度 |

第 0 步是 Go 语法速查，没有独立业务程序。第 5 步是实验课，复用第 4 步的功能，但目录里包含完整源码和可执行的故障实验。第 6 步的 h2c 是取消压测的协议实验，早期课程先使用 HTTP/1。

## 第一次怎么开始

```sh
cd lessons/01-proxy
go test ./...
```

然后按本课 README 启动 Mock、网关和 curl。第 1 课网关端口为 8081，第 2～7 课为 8080；切换课程前先停止已有服务。数据库、配置和实验输出都保存在当前课目录，避免混用。

各课是嵌套 Go 模块，仓库根目录的 `go test ./...` 只检查完整参考实现。检查所有课程必须在根目录运行：

```sh
bash scripts/verify-lessons.sh
bash scripts/verify-lessons.sh race
bash scripts/verify-lessons.sh build
```

仓库根目录的 cmd/internal 保留为完整参考实现；学习时先看课程目录。root 和课程快照今后的修改需要分别维护，课程是教学版本，不会自动同步根目录。
