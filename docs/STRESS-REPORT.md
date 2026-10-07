# Mock LLM + pprof 压测实测报告

## 持续取消连接异常修复（2026-10-07）

本轮仍在 Apple M2 Pro / macOS arm64 上运行独立负载端、网关和两个 Mock，使用与旧报告相同的 Go 1.26.8。没有修改系统网络参数、增加自动 POST 重试或把连接错误归类为预期取消。

旧协议 16 并发、12 秒的取消实验本轮未重现错误；延长并提高负载后重现连接异常。以下三轮**分别独立执行**，均为 64 个闭环 worker、60 秒、首块后取消、间隔 20ms，两个上游各有 32 个名额，保留 pprof 采样。早期两个 HTTP/1 压力任务曾同时运行，其结果不用于下面的隔离比较。

| 协议 / 地址及原始结果 | 已发请求 | 预期取消 | 传输错误 | 意外协议/HTTP 错误 | 客户端成功建连 | Mock A+B 接受连接 |
|---|---:|---:|---:|---:|---:|---:|
| [HTTP/1.1 / IPv4](stress-results/cancel-http1-20261007/result.json) | 64,774 | 33,077 | 30,665 | 1,032（502） | 33,093 | 33,039 |
| [h2c / IPv4](stress-results/cancel-h2c-ipv4-20261007/result.json) | 89,597 | 89,597 | 0 | 0 | 30 | 19 |
| [h2c / IPv6](stress-results/cancel-h2c-ipv6-20261007/result.json) | 89,951 | 89,951 | 0 | 0 | 45 | 22 |

HTTP/1 的错误样本为 `connect: can't assign requested address`。负载中同时记录到 [16,190 条 TIME_WAIT](stress-results/cancel-http1-20261007/tcp-states-during.txt) 和等待建立的连接；[系统临时端口范围](stress-results/cancel-http1-20261007/tcp-settings.txt) 为 49152～65535，TCP MSL 为 15000ms。结合 33,093 次客户端建连和约 33,000 次上游建连，本轮连接资源压力有实时证据支撑。三个角色共用同一台机器，不能把这个限制当成网关本身的吞吐上限。

HTTP/1 的流式请求在 body 未结束时取消，无法复用该连接；持续取消会反复建立 TCP。HTTP/2 可以只取消对应流，保留连接及其上的其他请求。实现采用 Go 标准库的 [Protocols 配置](https://go.dev/src/net/http/http.go) 与 [Transport](https://go.dev/src/net/http/transport.go)，没有引入新依赖。

修复与验证：

- 网关和 Mock 新增显式 `-h2c`，可同时接受 HTTP/1 和明文 HTTP/2。上游 endpoint 的 `"h2c": true` 使用独立、共享的 h2c client；普通 endpoint 的 HTTP/1 / HTTPS TLS 协商保持原配置。h2c 必须明确配置，不自动降级重发 POST。
- 压测默认只在 cancel 场景对三段链路都启用 h2c；`HTTP_PROTOCOL=http1` 保留旧协议对照。报告增加实际响应协议和客户端 DialContext 统计，Mock 增加真实接受连接数（也包含健康检查/采样连接）。HTTP/2 初始化时可以有多个并行建连，不能将表中的连接数理解成恰好一条。
- 两轮 h2c 的全部请求实际返回 HTTP/2，两个 Mock 的 `canceled` 合计分别为 89,597 和 89,951，`completed=0`，网关和两个 Mock 的 `active` 最终均为 0。不是仅在负载端计一次取消；取消到达了上游。网关排空后 goroutine 为 27（初始 25），仍保有可复用的上游连接；不据这个快照宣称所有协程已消失。
- 回归测试 `TestH2CCancelKeepsOtherStreamsAndConnection` 在一个正常活动流旁连续取消 128 个请求，验证取消到达上游、正常流得到完整 DONE、上下游各只建立一条 TCP、供应商名额最终归零。
- 取消压测启用 `-fail-on-error`。隔离 HTTP/1 对照以状态 2 失败，但仍保存结果及完成采样；两轮 h2c 以状态 0 完成。意外传输/协议错误没有被忽略，预期取消和容量拒绝单独计数。
- 全部 62 项顶层测试（另含子用例）、`go test -race`、`go vet`、构建及脚本语法检查通过。

复现当前取消场景与旧协议对照：

```sh
SCENARIOS=cancel CANCEL_CONCURRENCY=64 STRESS_DURATION=60s bash scripts/stress.sh
STRESS_HOST=::1 SCENARIOS=cancel CANCEL_CONCURRENCY=64 STRESS_DURATION=60s bash scripts/stress.sh
HTTP_PROTOCOL=http1 SCENARIOS=cancel CANCEL_CONCURRENCY=64 STRESS_DURATION=60s bash scripts/stress.sh
```

**适用范围：**持续取消导致的本机连接重建压力已通过 HTTP/2 路径消除，并通过 IPv4/IPv6 对照验证；仅支持 HTTP/1 的上游仍会在取消时关闭连接，须控制到达速率或使用独立负载机。真实 HTTPS HTTP/2 使用 TLS 协商，h2c 只用于明确支持它的本机/可信内网，不提供加密。旧 IPv6 轮次的 `socket is not connected` 未单独重现，不将本轮端口压力证据当成其精确根因。下面的历史失败结果全部保留。

## 本机成功吞吐上探（2026-09-25）

负载端、网关、两个 Mock Provider 仍在同一台机器上独立运行。为测量网关处理能力而非默认容量限制，本轮把网关一次性请求容量设为 4,096、两个端点各设为 2,048；Mock 一次性响应等待 20ms，关闭请求速率限制和 pprof。下表是**客户端并发 worker 数**与已完成的成功请求数，吞吐是整个测试窗口的平均值。

| 客户端 worker | 时长 / 启动方式 | 成功请求 | 成功吞吐 | 错误 |
|---:|---|---:|---:|---|
| [256](stress-results/ceiling/256/result.json) | 12 秒 / 同时启动 | 142,330 | 11,840 次/秒 | 0 |
| [512](stress-results/ceiling/512-after/result.json) | 12 秒 / 同时启动 | 278,803 | 23,190 次/秒 | 0 |
| [1,024](stress-results/ceiling/1024/result.json) | 12 秒 / 同时启动 | 456,262 | **37,956 次/秒** | 0 |
| [1,024](stress-results/ceiling/1024-ramp/result.json) | 20 秒 / 2 秒渐进启动 | 729,066 | 36,415 次/秒 | 0 |
| [1,152](stress-results/ceiling/1152-ramp/result.json) | 20 秒 / 2 秒渐进启动 | 269,385 | 13,447 次/秒 | 5,628 次客户端连接错误、2 次 502 |
| [1,536](stress-results/ceiling/1536-ramp/result.json) | 20 秒 / 2 秒渐进启动 | 163,132 | 6,141 次/秒 | 9,998 次客户端连接错误 |
| [2,048](stress-results/ceiling/2048-fresh/result.json) | 12 秒 / 新目标端口 | 30,488 | 2,494 次/秒 | 3,194 次客户端连接错误、1,552 次 502 |

本轮**最大无错误观测值**是 1,024 worker、12 秒内平均 37,956 次/秒；20 秒复测仍有 729,066 次成功且无错误，成功响应延迟 p95 为 37.6ms。1,152 worker 起，负载端大量报告 `connect: can't assign requested address`，即使换目标端口、渐进启动仍会复现。这个拐点是同机负载端的网络资源限制；现有结果不能推出网关程序或硬件的最终上限。1,024 worker 时，两个 Mock 各达到 512 个同时处理请求，说明本轮确实超过了原来的 64 路网关配置。

压测还发现上游 HTTP 空闲连接池原来固定为每主机 64 条。512 worker 时[调整前](stress-results/ceiling/512-before/result.json)有 1,715 次 502，成功吞吐 13,357 次/秒；按端点总容量调整连接池后，512 worker 的 502 消失，成功吞吐升至 23,190 次/秒。连接池不是流量许可额度；空闲上限只决定已建立连接能保留多少以供复用。

重跑 20 秒档位：`SCENARIOS=unary PROFILE=0 ENDPOINT_CAPACITY=2048 GATEWAY_UNARY_CAPACITY=4096 UNARY_CONCURRENCY=1024 STRESS_DURATION=20s STRESS_RAMP=2s bash scripts/stress.sh`。需要 Go 1.26；没有放进 PATH 时可用 `GO` 指定二进制。`PROFILE=0` 会关闭诊断服务和采样；延迟报告在样本超过 100,000 时使用覆盖全程的有界抽样。完整本机产物保存在被 Git 忽略的 `artifacts/`。

## 1 万闭环 worker 过载测试

2026-09-25 在本机以 10,000 个固定 worker 向本地网关持续发送 12 秒一次性请求；两个 Mock Provider 各等待 20ms。网关的一次性并发上限仍为 64，入口处理上限为 160，因此本次验证的是大批并发请求到达时的过载保护，不是 1 万路同时推理。请求统计见[原始 JSON](stress-results/scale-10k/result.json)。

| 请求启动 | 成功 | 网关 503 | 客户端传输错误 | 成功延迟 p50 / p95 / p99 | 成功 RPS |
|---:|---:|---:|---:|---:|---:|
| 101,162 | 6,493 | 91,633 | 3,036 | 1,444 / 1,979 / 2,017 ms | 529.78 |

网关和 Mock 进程都保持运行并正常退出，结果同时暴露了负载机瓶颈：传输错误样本是压测客户端 `dial tcp ... connect: can't assign requested address`，说明单机单源地址无法稳定承载这一级别的连接建立/重建速率。因而这组数据证明了 10,000 worker 下网关仍能拒绝超额请求并完成部分请求，但不应用来估计生产吞吐或 10,000 条长连接容量；要测 100,000 级别或测网关极限，需要多台负载机分摊连接与源端口，并按生产容量调整网关、上游和速率配置。

重跑命令：`SCENARIOS=unary PROFILE=0 UNARY_CONCURRENCY=10000 STRESS_DURATION=12s OUT=artifacts/stress-scale-10k bash scripts/stress.sh`。压测报告落入 `artifacts/`，该目录被 Git 忽略；`PROFILE=0` 关闭 pprof 采样，减少高负载下的诊断开销。

日期：2026-09-24。环境：Apple M2 Pro、macOS arm64、Go 1.26.8。网关、负载客户端、两个 Mock LLM 分别运行于独立进程，但共用一台机器。所有模型调用均为 Mock，没有付费模型或真实业务数据。

**结论：一次性、长流、有界超载、带备用余量的 fallback、异步持久任务和预期停流释放均完成验证；高频主动取消仍存在连接异常，不能宣称所有压力场景通过。**

## 请求结果

调度新请求 12 秒；已开始的流允许继续完成。成功吞吐按实际总观测时长计算，包括尾部请求和异步查询排空时间。P95 只针对成功请求（任务为从提交至数据库 done 的端到端时间，取消为首块到达后取消的请求耗时），不混入快速 503。没有成功样本时 0 只是无样本，不能理解为零延迟。

| 场景及原始结果 | 客户端并发 | 有效结果 | 503/429 拒绝 | 非预期错误* | P95(ms) | 实际完成/s |
| --- | ---: | --- | ---: | ---: | ---: | ---: |
| [一次性](stress-results/unary/result.json) | 64 | 32,662 / 32,662 | 0 | 0 | 27.50 | 2720.33 |
| [10秒长流](stress-results/long-stream/result.json) | 64 | 128 / 128 | 0 | 0 | 10132.20 | 6.32 |
| [超载](stress-results/overload/result.json) | 192 | 128 / 69,124 | 68,996 | 0 | 10140.67 | 6.32 |
| [主动取消](stress-results/cancel/result.json) | 16 | 2,320 次主动取消 | 0 | 9 | 29.66 | — |
| [fallback（有余量）](stress-results/fallback/result.json) | 16 | 8,141 / 8,141 | 0 | 0 | 26.64 | 677.36 |
| [异步任务](stress-results/jobs/result.json) | 32 | 4,595 done / 4,595 接受 | 16,557 | 0 | 724.17 | 359.99 |
| [停流超时](stress-results/timeout/result.json) | 16 | 0 / 64 | 0 | 64（预期截断） | 0.00 | — |

\* 停流场景故意省略后续数据，因此 64 次传输截断属于预期，不能算成成功回答。主动取消的 9 次错误是真实建连超时，保留为未解决项。单轮测试不是生产容量承诺。

- 长流共完成 128 个请求，总用时 20.25 秒，首块 P95 **110.82ms**，完整响应 P95 **10.13 秒**。把 128 除以调度窗口 12 秒会高估完成吞吐，本报告没有这么计算。
- 超载时 192 个客户端竞争 64 个流式名额，返回 **68,996 次明确拒绝**；两个端点各自观测到的名额峰值均为 32，未突破配置。正常流仍能结束。
- fallback 余量场景的 Mock A 记录 **1,452 次 503**，客户端 **8,141 次全部成功**，两个上游合计调用 9,593 次，差值正好是 fallback 次数。
- 异步任务接受 **4,595** 个，全部到达 `done`，失败和剩余均为 0。直接以只读 SQL 检查实验数据库，同样得到 `done=4595`；并非只统计 HTTP 202。任务端到端 P95 **724.17ms**，接受接口 P95 **10.72ms**。
- 停流场景发出首块后停止，3 秒 idle timeout 触发截断。Mock 的 64 次调用均记录取消，网关及上游 active 最终都为 0。

## pprof 看到了什么

### 长流占用随生命周期释放

来自 [原始快照目录中的 settled 统计](stress-results/long-stream/settled-stats.json)：

| 时刻 | goroutine 数 | GC 后堆分配量 | 网关在途 |
| --- | ---: | ---: | ---: |
| 开始前 | 25 | 3.28 MiB | 0 |
| 负载中快照 | 284 | 8.75 MiB | 64 |
| 请求排空后 | 153 | 5.17 MiB | 0 |
| 再等待 95 秒 | 25 | 3.90 MiB | 0 |

负载中数据是一次采样，不代表绝对内存峰值；这里是 Go heap，不是进程 RSS。Transport 保留空闲连接 90 秒，所以排空后仍有连接读写 goroutine。等待回收后回到基线，说明本轮没有观察到活动流协程残留；不能据单轮实验证明永远无泄漏。所有七类保留的完成采样中，网关 active 和两个 Mock active 最终均为 0。

[goroutine 栈摘要](stress-results/long-stream/settled-goroutine-top.txt) 中保留的主要是 16 个固定 worker、HTTP listener、数据库连接管理和诊断采样自身。

### 一次性请求主要花在 HTTP I/O 与分配

[CPU top](stress-results/unary/cpu-top.txt) 的 10.16 秒采样窗收集到 4.45 秒 CPU 样本；`syscall.rawsyscalln` 占样本的 65.39%。这里的比例是 CPU 样本占比，不是端到端延迟占比，也不是整机 CPU 利用率。

[累计分配 top](stress-results/unary/allocs-top.txt) 显示约 425.70MB 累积分配，热点包括 `io.ReadAll`、HTTP header、JSON 解码和 context。这些包含已经回收的对象，不意味着内存泄漏。当前没有证据要求把短路由锁改成复杂无锁结构。

### 异步入口存在明确的串行化等待

[mutex top](stress-results/jobs/mutex-top.txt) 中 `Store.Submit` 相关调用链贡献了约 33.62 秒累计锁竞争延迟。[block top](stress-results/jobs/block-top.txt) 同时显示提交锁和数据库连接等待。

多个 goroutine 的等待时间会相加，33.62 秒不是一个请求等了 33.62 秒。当前 Submit 用互斥锁串行执行容量检查和写事务，SQLite 只有一个连接；这保证教学实现简单，但高并发提交会在这里排队。若继续提升异步吞吐，应先评估批量写入、缩短事务与公平调度；多副本再考虑支持租约的数据库队列，而不是盲目增加 worker 数。

## 压测中修正的内容

1. 负载客户端现在为拒绝、成功、任务接受和主动取消分别统计延迟，并用实际总时间计算完成率。
2. 错误响应先有界读取再关闭；客户端的全局和单 host 空闲连接容量匹配并发，所有 HTTP 错误退让 20ms，防止工具制造无意义的重连风暴。
3. 网关上游错误体改为**最多 4KiB、最多 100ms**的有界排空。新增测试证明连续 10 次小 503 共用一个连接，而卡住的错误体仍及时释放。这是压测发现的连接复用改进，不代表修好了所有取消网络异常。
4. 候选耗尽时的上游 503 保留为 503 并带 Retry-After，避免误映射成 502。
5. 修正了 macOS Bash 3 在空 PID 数组下退出清理的兼容问题；清理只针对脚本自己的子进程。

满载 fallback 的前置复测也保留在 [修复错误体复用后的满载结果](stress-results/fallback-saturated/result.json)：64 并发时，33,961 次请求有 31,281 次成功和 2,680 次失败。当时上游 503 仍被映射成 502，随后已修正映射。Mock A 的 7,166 次故障中，只有 4,486 次取得了额外候选调用，剩余 2,680 次无法被备用容量吸收。**fallback 不能凭空增加容量。**有余量的 16 并发复测与这个满载场景用途不同，不能直接拿吞吐数字比较优化幅度。

## 历史未解决记录：高频取消产生本机连接异常（2026-09-24）

持续取消的 HTTP/2 修复与 2026-10-07 复测见本文开头。以下为原始 HTTP/1 轮次的发现，不以新结果覆盖旧故障，也不宣称已证明旧 IPv6 错误的精确根因。

- 第一轮 IPv6 高取消流量未能完成采样，没有可用的请求结果文件；因此不将其作为成功数据。
- 调整连接池后的 IPv6 16 并发取消仍出现 `read: socket is not connected`，并伴随 502；见 [原始结果](stress-results/cancel-ipv6/result.json)。
- 显式 IPv4 下完成 2,320 次主动取消，但仍有 9 次 `dial tcp ...: i/o timeout`；见 [IPv4 原始结果](stress-results/cancel/result.json)。这不是预期取消，不能从错误数中扣掉。
- 已确认 2,320 次已接收取消均到达 Mock，两个上游 active=0，网关 active=0、goroutine 回到 25。因此“资源释放有效”和“高频重连仍异常”是两项独立结论。
- 事后 TCP 检查不足以证明是端口耗尽；IPv4 也复现，因此也不能认定是 IPv6 专属问题。当前未改操作系统网络参数、未证明根因。下一步应使用独立压测主机、连接事件追踪或抓包，区分本机网络环境、连接重建速率与实现问题。

## 复现、产物与验证

执行 [scripts/stress.sh](../scripts/stress.sh)，步骤及指标解释见 [第六课](06-pprof.md)。默认七场景全部运行；`SCENARIOS='jobs timeout'` 可以只运行指定部分，`FALLBACK_CONCURRENCY=64` 可以观察备用容量不足。

报告使用了多轮采集：正常一次性/长流来自首轮；超载来自修正负载连接池后的轮次；取消保留 IPv4 故障复现；jobs/timeout 单独隔离采集；fallback 有余量的最终轮次使用最终网关实现。没有把早期失败轮次删掉或混成一次全部通过的结果。

完整 `.pb.gz` profile、符号化二进制、日志和 SQLite 文件留在本地 Git 忽略的 `artifacts/`。仓库发布的 `docs/stress-results/` 保留对应的 JSON 请求统计、关键运行时快照和文本 pprof top，方便先阅读并按教程重跑完整采样。调试端口只绑定回环地址，压测也核实业务端口访问 `/debug/pprof/` 返回 404。

最终自动验证：49 项顶层测试，`go test -race`、`go vet`、构建和脚本语法检查通过。自动测试通过不覆盖或抹去上述真实压力测试失败。所有压测进程已停止。所有模型调用均使用本地 Mock。
