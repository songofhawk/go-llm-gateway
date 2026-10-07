# 第 6 课：Mock、压测与 pprof

本目录是一份独立、可运行的代码版本，包含学到本课为止的功能：独立诊断端口、压测客户端、采样脚本、可选 h2c。

这里有自己的 `go.mod`、源码、Mock 和配置；所有 import 都指向本目录内的包。可以单独复制本目录运行。Go 需要 1.26 或更高版本。从第 4 课开始使用纯 Go SQLite 驱动，首次运行需要下载 go.mod 中的依赖。

## 本课要理解什么

在第 5 课之上新增三块代码：[diagnostics.go](cmd/gateway/diagnostics.go)、[loadtest](cmd/loadtest/main.go)、[stress.sh](scripts/stress.sh)。业务代码的 `Gateway.Snapshot` 供诊断端口报告 active/capacity/peak。

pprof 默认关闭，用 `-pprof localhost:6060` 在单独的本机端口开启。loadtest 是产生有界负载的客户端，Mock 是模拟供应商，pprof 只采样网关进程。

先跑较短的单场景，再阅读采样：

```sh
SCENARIOS=unary STRESS_DURATION=3s bash scripts/stress.sh
```

脚本构建本课自己的三个程序，启动自己的子进程，保存报告和采样到 `artifacts/`，退出时停止它创建的进程。默认业务/诊断/Mock 端口分别是 18080/16060/19090/19091；可通过 `GW_PORT`、`DEBUG_PORT`、`MOCK_A_PORT`、`MOCK_B_PORT` 调整。

先看 `result.json` 的成功、拒绝和延迟，再看 CPU、heap、goroutine、block、mutex。长流默认实验包含等待连接池回收的 95 秒，第一次用 unary 会更容易观察。

持续取消场景默认在上下游启用 h2c，取消一个 HTTP/2 流可以保留 TCP 连接。`-h2c` 和配置的 `h2c` 需要链路两端配套；这是可信本机实验选项。本课没有请求速率桶。

## 独立运行

从仓库根目录进入本课；下面的 go/curl 命令均以本课目录为起点。每次先停止上一课服务，因为各课会使用相同端口。

```sh
cd lessons/06-pprof
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
diff -ru lessons/05-labs/internal lessons/06-pprof/internal
diff -ru lessons/05-labs/cmd/gateway lessons/06-pprof/cmd/gateway
```

配套原理文档：[第 6 课](../../docs/06-pprof.md)。 下一份独立版本：[07-rate-limiting](../07-rate-limiting/README.md)。
