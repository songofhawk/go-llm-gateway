# 第 7 课：请求速率限流

本目录是一份独立、可运行的代码版本，包含学到本课为止的功能：令牌桶、rate/burst、入口 429、共享额度。

这里有自己的 `go.mod`、源码、Mock 和配置；所有 import 都指向本目录内的包。可以单独复制本目录运行。Go 需要 1.26 或更高版本。从第 4 课开始使用纯 Go SQLite 驱动，首次运行需要下载 go.mod 中的依赖。

## 本课要理解什么

在第 6 课之上新增 [ratelimit.go](internal/gateway/ratelimit.go)，HTTP 层在读取正文前检查令牌桶，main 增加 rate/burst 参数。

```sh
go run ./cmd/gateway -rate 2 -burst 3
```

刚启动有 3 个令牌，可放行 3 个几乎同时到达的新请求，之后每秒补 2 个。没有令牌立即返回入口 429 和 Retry-After。令牌按请求扣一次，失败不退；SSE 分块、fallback、任务查询和 worker 执行不会重复扣入口令牌。

限流控制新请求进入速度；并发名额控制尚未结束的工作。聊天提交与异步提交共享一个单进程桶，默认 `-rate 0 -burst 0` 关闭。

阅读顺序：[ratelimit.go](internal/gateway/ratelimit.go) 的 `Allow` → [http.go](internal/gateway/http.go) 的 `Handler` → [main.go](cmd/gateway/main.go) 的参数组装。

```sh
go test -race -v ./internal/gateway -run TestRateLimit
go run ./cmd/loadtest -url http://localhost:8080 -mode unary \
  -duration 3s -concurrency 8 -output result.json
```

观察报告的 `http_status_counts`，区分入口 429、容量 503 和上游错误。受控时钟测试还会精确验证补充时间与并发扣令牌的上界。

## 独立运行

从仓库根目录进入本课；下面的 go/curl 命令均以本课目录为起点。每次先停止上一课服务，因为各课会使用相同端口。

```sh
cd lessons/07-rate-limiting
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
diff -ru lessons/06-pprof/internal lessons/07-rate-limiting/internal
diff -ru lessons/06-pprof/cmd/gateway lessons/07-rate-limiting/cmd/gateway
```

配套原理文档：[第 7 课](../../docs/07-rate-limiting.md)。 本课包含教程的全部网关功能。
