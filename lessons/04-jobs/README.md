# 第 4 课：持久化后台任务

本目录是一份独立、可运行的代码版本，包含学到本课为止的功能：SQLite、任务状态机、固定 worker、重启恢复。

这里有自己的 `go.mod`、源码、Mock 和配置；所有 import 都指向本目录内的包。可以单独复制本目录运行。Go 需要 1.26 或更高版本。从第 4 课开始使用纯 Go SQLite 驱动，首次运行需要下载 go.mod 中的依赖。

## 本课要理解什么

增加一种新的交付方式：先持久化任务并返回 202，再由固定 worker 执行，客户端用任务 ID 查询。

状态依次为 `queued → running → completed → processing → done`；业务执行错误进入 `failed`。数据库先保存模型结果，再进行本地后处理。重启时 `running` 重新入队，`processing` 回到 `completed`。因此模型调用可能重复，恢复保证是至少一次。

提交的 HTTP context 只负责入库；后台工作使用 worker context，所以 202 返回后任务会继续。停机取消 worker 并等待退出。一个 SQLite 文件只供一个网关进程独占。

阅读顺序：[http.go](internal/gateway/http.go) 的 `submit` / `getJob` / `ExecuteJob` → [jobs.go](internal/jobs/jobs.go) 的 `Submit` / `Run` / `saveResult` / `claimCompleted` → [main.go](cmd/gateway/main.go) 的 worker 组装与退出。

启动时可观察小队列：

```sh
go run ./cmd/gateway -db jobs-lab.sqlite -workers 2 -queue 8
```

另开终端提交与查询：

```sh
curl -i http://localhost:8080/v1/jobs -H 'Content-Type: application/json' \
  -d '{"model":"balanced","messages":[{"role":"user","content":"后台生成报告"}]}'
curl http://localhost:8080/v1/jobs/返回的任务ID
```

保持 Mock 运行，用同一个数据库路径停止并重启网关，观察未完成任务恢复。更精确的恢复窗口由 `TestRecoveryRequeuesRunningAndPromotesSavedResult` 验证。

## 独立运行

从仓库根目录进入本课；下面的 go/curl 命令均以本课目录为起点。每次先停止上一课服务，因为各课会使用相同端口。

```sh
cd lessons/04-jobs
go test ./...
```

Mock 是本地模拟服务，只返回固定 `x` 文本，用来观察协议、时间和故障，不调用真实模型，也不需要 API 账号。Mock 工具的故障选项供后续实验复用，初学时先读网关代码。

在三个不同终端进入本课目录，再各运行一条：

```sh
go run ./cmd/mock-provider -addr localhost:9090
go run ./cmd/mock-provider -addr localhost:9091
go run ./cmd/mock-provider -addr localhost:9092
```

第四个终端启动本课网关：

```sh
go run ./cmd/gateway -config config.example.json
```

第五个终端发请求：

```sh
curl -i http://localhost:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"economy","messages":[{"role":"user","content":"介绍一下 Go"}],"stream":false}'
curl -N -i http://localhost:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"economy","messages":[{"role":"user","content":"介绍一下 Go"}],"stream":true}'
```

观察 `X-Gateway-Provider` 和 `X-Gateway-Model`：客户端的逻辑档次已被映射到真实模型。默认监听本机；如设置 `GATEWAY_TOKEN`，每条网关请求还需加 `Authorization: Bearer ...`。

## 验证与对照

本课的测试文件与源码放在一起，只引用本课的包。运行 `go test -race ./...` 可进一步检查并发行为。

从仓库根目录比较前一课与本课：

```sh
diff -ru lessons/03-concurrency/internal lessons/04-jobs/internal
diff -ru lessons/03-concurrency/cmd/gateway lessons/04-jobs/cmd/gateway
```

配套原理文档：[第 4 课](../../docs/04-jobs.md)。 下一份独立版本：[05-labs](../05-labs/README.md)。
