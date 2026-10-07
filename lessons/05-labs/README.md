# 第 5 课：故障实验

本目录是一份独立、可运行的代码版本，包含学到本课为止的功能：沿用第 4 课功能，新增可执行故障实验与观察清单。

这里有自己的 `go.mod`、源码、Mock 和配置；所有 import 都指向本目录内的包。可以单独复制本目录运行。Go 需要 1.26 或更高版本。从第 4 课开始使用纯 Go SQLite 驱动，首次运行需要下载 go.mod 中的依赖。

## 本课要理解什么

本课的网关源码与第 4 课一致：这一课训练如何制造故障、检查失败表现、确认资源释放，不引入新的网关机制。所有源码和测试都已复制到本目录，实验直接针对这里的实现执行。

[scripts/labs.sh](scripts/labs.sh) 提供可执行的实验入口。实验使用真实的临时 HTTP 服务或 SQLite 文件，不要求你先启动默认端口的服务；每次实验结束会清理测试资源。

```sh
bash scripts/labs.sh streaming   # 首块提前到达
bash scripts/labs.sh fallback    # 503 换候选，400 不换
bash scripts/labs.sh cancel      # 客户端断开，上游取消
bash scripts/labs.sh capacity    # 长流持有名额，超量拒绝
bash scripts/labs.sh timeout     # 区分各类时限
bash scripts/labs.sh truncation  # 缺少 DONE 不能冒充完成
bash scripts/labs.sh slow        # 慢客户端不会无限占住资源
bash scripts/labs.sh recovery    # running/processing 的恢复差异
bash scripts/labs.sh jobs        # 提交、执行、持久化、查询
bash scripts/labs.sh all         # 一次执行上述实验
```

先读脚本选择的测试，再看测试制造的故障，最后回到源码找保证该行为的规则。每个实验写下三个答案：客户端得到什么？上游是否停止？下一次请求能否成功？`PASS` 表示测试中的断言成立，不代表所有生产场景已经覆盖。

如果想手工观察，三个 Mock 都支持 `-status 503`、`-delay 10s`、`-stall-after 0`、`-truncate`；先修改一个服务观察 fallback，再让全部服务故障观察最终失败。

## 独立运行

从仓库根目录进入本课；下面的 go/curl 命令均以本课目录为起点。每次先停止上一课服务，因为各课会使用相同端口。

```sh
cd lessons/05-labs
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
diff -ru lessons/04-jobs/internal lessons/05-labs/internal
diff -ru lessons/04-jobs/cmd/gateway lessons/05-labs/cmd/gateway
```

第 4→5 课的网关 diff 应为空；新增内容在 scripts/labs.sh 与本课实验说明。

配套原理文档：[第 5 课](../../docs/05-labs.md)。 下一份独立版本：[06-pprof](../06-pprof/README.md)。
