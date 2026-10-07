# 第 2 课：模型路由与 fallback

本目录是一份独立、可运行的代码版本，包含学到本课为止的功能：逻辑模型档次、Provider 接口、同组轮转、有限 fallback。

这里有自己的 `go.mod`、源码、Mock 和配置；所有 import 都指向本目录内的包。可以单独复制本目录运行。Go 需要 1.26 或更高版本。本课只使用标准库。

## 本课要理解什么

这一课把第一课的一个 ReverseProxy 拆成 HTTP、路由、供应商协议三层。

`model=economy` 是客户端提出的逻辑档次；`config.example.json` 把它映射到 `demo-small` 等真实模型。同组先轮转选择一个候选，408/429/5xx 或网络错误可尝试下一个；其他 4xx 直接失败。每个候选最多尝试一次，整个调用和单候选都有时间预算。

SSE 先读取首块再交给 HTTP 层；首块交付后出现故障就中止，不能拼接备用模型的回答。JSON 在有界缓冲中验证后才交付。这里保留字段透传、连接复用、取消和认证。

这一阶段尚未统计供应商在途数量，也没有容量门或读写空闲时限。因此轮转不考虑哪个端点更忙。第 3 课把 `choose` 扩展成同时选择并占位的 `acquire`。

阅读顺序：[types.go](internal/gateway/types.go) → [provider.go](internal/gateway/provider.go) → [router.go](internal/gateway/router.go) 的 `choose` / `Do` → [http.go](internal/gateway/http.go) 的 `chat` → [main.go](cmd/gateway/main.go)。配置中没有 `capacity` 字段。

验证路由和错误边界：

```sh
go test -v ./internal/gateway -run 'TestRoundRobin|TestFallback|TestAttempt|TestProvider|TestTruncated'
```

## 独立运行

从仓库根目录进入本课；下面的 go/curl 命令均以本课目录为起点。每次先停止上一课服务，因为各课会使用相同端口。

```sh
cd lessons/02-routing
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

第 1 课使用一个 main.go 和标准库 ReverseProxy，第 2 课开始拆出 internal/gateway；两者的结构变化较大，按阅读顺序逐个文件对照。

配套原理文档：[第 2 课](../../docs/02-routing.md)。 下一份独立版本：[03-concurrency](../03-concurrency/README.md)。
