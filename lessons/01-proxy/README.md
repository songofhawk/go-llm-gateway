# 第 1 课：最小代理

本目录是一份独立、可运行的代码版本，包含学到本课为止的功能：固定上游、JSON/SSE 转发、请求取消。

这里有自己的 `go.mod`、源码和 Mock；上游地址通过环境变量配置；所有 import 都指向本目录内的包。可以单独复制本目录运行。Go 需要 1.26 或更高版本。本课只使用标准库。

## 本课要理解什么

从 `main.go` 的 `main` 开始：读取上游地址 → 创建 Transport → 启动 HTTP Server。收到请求后进入 `ServeHTTP`，最后调用标准库 ReverseProxy。

本课的 `model` 是直接发给上游的真实模型 ID。它还没有逻辑档次、路由、并发名额、后台任务、pprof 或令牌桶。流式使用 ReverseProxy 的刷新机制；精细的读写空闲时限从第 3 课开始。

阅读顺序：[main](main.go) → `ServeHTTP` → `newGateway` 的 `Director` / `FlushInterval`。测试位于 [main_test.go](main_test.go)。

## 独立运行

从仓库根目录进入本课；下面的 go/curl 命令均以本课目录为起点。每次先停止上一课服务，因为各课会使用相同端口。

```sh
cd lessons/01-proxy
go test ./...
```

Mock 是本地模拟服务，只返回固定 `x` 文本，用来观察协议、时间和故障，不调用真实模型，也不需要 API 账号。Mock 工具的故障选项供后续实验复用，初学时先读网关代码。

在三个不同终端进入本课目录，再各运行一条：

```sh
go run ./cmd/mock-provider -addr localhost:9090
UPSTREAM_URL=http://localhost:9090/v1 UPSTREAM_API_KEY=demo-only go run .
curl -i http://localhost:8081/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"demo-small","messages":[{"role":"user","content":"介绍一下 Go"}],"stream":false}'
```

将 `stream` 改成 `true`，并给 curl 加 `-N`，即可看见逐块返回。第 1 课只有一个 Mock 端点，默认网关端口是 8081。

## 验证与对照

本课的测试文件与源码放在一起，只引用本课的包。运行 `go test -race ./...` 可进一步检查并发行为。

配套原理文档：[第 1 课](../../docs/01-proxy.md)。 下一份独立版本：[02-routing](../02-routing/README.md)。
