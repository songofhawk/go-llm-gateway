# 第 1 课：最小代理

本目录是一份独立、可运行的代码版本，包含学到本课为止的功能：固定上游、JSON/SSE 转发、请求取消。

这里有自己的 `go.mod`、源码和 Mock；上游地址通过环境变量配置；所有 import 都指向本目录内的包。可以单独复制本目录运行。Go 需要 1.26 或更高版本。本课只使用标准库。

## 本课要理解什么

完整代码导读：[第 1 课：从 main 读懂一个 Go 代理](../../docs/01-proxy.md)。正文包含对象关系图、启动和请求调用序列图，并对应解释结构体、方法、回调与请求生命周期。

先看 `server.Handler → *gateway → gateway.proxy → *httputil.ReverseProxy → Transport` 的关系，再从 `main` 跟到请求处理：标准库调用外层 `gateway.ServeHTTP`，它检查和恢复请求体后，再调用内层 `ReverseProxy.ServeHTTP`。

本课的 `model` 是直接发给上游的真实模型 ID。它还没有逻辑档次、路由、并发名额、后台任务、pprof 或令牌桶。流式使用 ReverseProxy 的刷新机制；精细的读写空闲时限从第 3 课开始。

阅读顺序：[main.go](main.go) 的 `main` → `newGateway` 如何组装对象 → `gateway.ServeHTTP` 如何处理请求 → 回到 `newGateway` 理解 `Director` / `ModifyResponse` / `ErrorHandler`。Mock 位于 [cmd/mock-provider/main.go](cmd/mock-provider/main.go)，测试位于 [main_test.go](main_test.go)。

## 独立运行

从仓库根目录进入本课；下面的 go/curl 命令均以本课目录为起点。每次先停止上一课服务，因为各课会使用相同端口。

```sh
cd lessons/01-proxy
go test ./...
```

Mock 是本地模拟服务，流式响应会逐块返回 `block1`、`block2` 等可辨认文本；普通响应会把这些块拼成一个完整文本。它用来观察协议、时间和故障，不调用真实模型，也不需要 API 账号。Mock 工具的故障选项供后续实验复用，初学时先读网关代码。

在三个不同终端进入本课目录：前两条命令分别启动 Mock 和网关，两条 curl 命令在终端三依次运行。

```sh
go run ./cmd/mock-provider -addr localhost:9090 -delay 700ms
UPSTREAM_URL=http://localhost:9090/v1 UPSTREAM_API_KEY=demo-only go run .
curl -i -sS -w '\n首字节：%{time_starttransfer}s，总耗时：%{time_total}s\n' \
  http://localhost:8081/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"demo-small","messages":[{"role":"user","content":"介绍一下 Go"}],"stream":false}'
curl -N -i -sS -w '\n首字节：%{time_starttransfer}s，总耗时：%{time_total}s\n' \
  http://localhost:8081/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"demo-small","messages":[{"role":"user","content":"介绍一下 Go"}],"stream":true}'
```

第一条是一次性响应，第二条是流式响应；后者的 `-N` 会关闭 curl 的输出缓冲，让 `block1`、`block2` 逐块显示。比较两次输出中的首字节时间和总耗时。Mock 默认模拟 5 个生成步骤：一次性模式等约 5 × 700ms 后完整返回，流式模式约 700ms 后先返回第一块，再逐块输出；总生成时间接近。第 1 课只有一个 Mock 端点，默认网关端口是 8081。

## 验证与对照

本课的测试文件与源码放在一起，只引用本课的包。运行 `go test -race ./...` 可进一步检查并发行为。

完整代码导读：[第 1 课](../../docs/01-proxy.md)。 下一份独立版本：[02-routing](../02-routing/README.md)。
