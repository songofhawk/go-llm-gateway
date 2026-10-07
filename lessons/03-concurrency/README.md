# 第 3 课：并发与生命周期

本目录是一份独立、可运行的代码版本，包含学到本课为止的功能：入口及供应商容量、按负载选择、读写超时、优雅停机。

这里有自己的 `go.mod`、源码、Mock 和配置；所有 import 都指向本目录内的包。可以单独复制本目录运行。Go 需要 1.26 或更高版本。本课只使用标准库。

## 本课要理解什么

以第 2 课为基础，给“还没有结束的工作”设置上限。

`Endpoint.Capacity` 限制供应商的总在途请求；HTTP 入口、stream 和 unary 各有名额。路由在短锁内按占用比例选择并增加计数，满载立即拒绝。流式名额一直保留到 `Result.Body.Close`，收到响应头不会提前归还。

`idleBody` 限制单次上游读取；`ResponseController` 限制单次下游写入。客户端取消会传递到上游。入口捕获停止信号，先停止接收新请求，最多给前台 5 秒完成，之后取消剩余请求。本课没有数据库或 worker。

阅读顺序：[router.go](internal/gateway/router.go) 的 `acquire` / `ownedBody` / `idleBody` → [http.go](internal/gateway/http.go) 的 `acquireGate` / `chat` / `relaySSE` → [main.go](cmd/gateway/main.go) 的停机逻辑。

观察容量与取消：

```sh
go run ./cmd/gateway -streams 1 -unary 1 -idle-timeout 2s
go test -race -v ./internal/gateway -run 'TestLoadBalance|TestConcurrent|TestCancellation|TestSlow|TestShutdown'
```

保持一个长流运行时再发另一个流，会遇到入口容量 503；结束第一个流后可再次成功。供应商容量在 `config.example.json` 中调整。

## 独立运行

从仓库根目录进入本课；下面的 go/curl 命令均以本课目录为起点。每次先停止上一课服务，因为各课会使用相同端口。

```sh
cd lessons/03-concurrency
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
diff -ru lessons/02-routing/internal lessons/03-concurrency/internal
diff -ru lessons/02-routing/cmd/gateway lessons/03-concurrency/cmd/gateway
```

配套原理文档：[第 3 课](../../docs/03-concurrency.md)。 下一份独立版本：[04-jobs](../04-jobs/README.md)。
