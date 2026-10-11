# 第 4 课：会话粘性路由

本目录是一份独立、可运行的代码版本，累计第 3 课的容量、超时、取消和停机机制，新增固定首选的会话软粘性。只使用标准库，没有数据库、后台 worker、pprof、h2c 或令牌桶。

这里有自己的 `go.mod`、源码、Mock 和配置；所有 import 都指向本目录内的包。可以单独复制本目录运行。Go 需要 1.26 或更高版本。

## 本课要理解什么

请求可选携带 `gateway_session`：非空白字符串，最多 256 个 UTF-8 字节。调用方为会话建立稳定且带用户/租户命名空间的键，或使用全局唯一的随机会话 ID。这个键用于路由，不是鉴权凭据。

同一会话、同一档次，通过最高随机权重哈希确定组内首选 `Provider + Model`。首选有容量就继续使用；满载或失败时临时 fallback，下次请求仍优先原上游。没有会话字段时，沿用第 3 课的按占用比例选择。流式输出后不能切换，所有调用仍受容量和时间预算约束。

阅读顺序：[types.go](internal/gateway/types.go) 的 `Session` / `ParseRequest` → [affinity.go](internal/gateway/affinity.go) 的 `sessionTarget` → [router.go](internal/gateway/router.go) 的 `Do` / `acquire` → [provider.go](internal/gateway/provider.go) 的字段过滤。原始会话字段留在请求中，发给供应商前才移除。

本课不保存 session 绑定表，没有 TTL 或“上次成功上游”状态。相同配置和会话键能跨重启确定相同首选；实时容量与故障仍可能使实际选择不同。

## 独立运行

先停止上一课服务。在仓库根目录执行：

```sh
cd lessons/04-session-affinity
go test ./...
```

在三个不同终端进入本课目录，分别启动 Mock：

```sh
go run ./cmd/mock-provider -addr localhost:9090
go run ./cmd/mock-provider -addr localhost:9091
go run ./cmd/mock-provider -addr localhost:9092
```

第四个终端启动网关：

```sh
go run ./cmd/gateway -config config.example.json
```

第五个终端连续发送同一会话的请求：

```sh
for round in 1 2 3; do
  curl -sS -i http://localhost:8080/v1/chat/completions \
    -H 'Content-Type: application/json' \
    -d '{"model":"balanced","gateway_session":"tenant-1/user-2/session-3","messages":[{"role":"user","content":"介绍一下 Go"}]}'
done
```

观察 `X-Gateway-Provider` / `X-Gateway-Model`。上游正常且有容量时，两者应固定。把 `stream` 改为 `true` 并给 curl 加 `-N`，仍应使用同一个首选；删除 session 字段则恢复普通负载均衡。设置了 `GATEWAY_TOKEN` 时，调用需带 `Authorization: Bearer 你的令牌`。

Mock 不调用真实模型，也没有模型前缀缓存；本课验证路由稳定性，不测真实缓存收益。

## 验证与对照

```sh
go test -v ./internal/gateway -run TestSession
go test -race ./...
```

测试覆盖相同会话在其他流量干扰、新实例、候选重排后的稳定性，不同会话的分散，实际模型绑定，满载临时切换、容量释放后返回首选，429/500/400 和有限 fallback，以及 HTTP JSON/SSE 和扩展字段隔离。原有普通负载均衡、超时、取消与截断测试继续保留。

从仓库根目录比较前一课与本课：

```sh
diff -ru lessons/03-concurrency/internal lessons/04-session-affinity/internal
diff -ru lessons/03-concurrency/cmd/gateway lessons/04-session-affinity/cmd/gateway
```

配套原理文档：[第 4 课](../../docs/04-session-affinity.md)。下一份独立版本：[05-jobs](../05-jobs/README.md)。
