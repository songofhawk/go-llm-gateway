---
message: "Go LLM Gateway 如何把模型调用变成可控的路由、流式、并发与后台任务"
audience: 正在学习 Go 服务端开发、希望理解 LLM 网关实现的开发者
aspect: 1920x1080
language: zh-CN
---

## Rhythm

慢钩子 → 快速建立学习地图 → 端到端请求 → 逐层放大两个关键边界（资源生命周期、任务所有权）→ 用限流完成概念对照 → 稍慢地给出本地上手路线。动态图形表达关系，语音给出解释；每个术语至少用一次实际请求或资源状态落地。

## Frame 1

- status: outline
- src: compositions/01-opening.html
- rules: `center-outward-expansion`, `svg-path-draw`
- beat: 一个问题穿过 Go 网关，模型只在右侧出现；网关把“选谁、等多久、何时释放资源”变成显式规则。

## Frame 2

- status: outline
- src: compositions/02-roadmap.html
- rules: `stat-bars-and-fills`, `center-outward-expansion`
- beat: 从第 0 课 Go 基础到第 7 课令牌桶，路线由语法、请求、路由、并发、任务、实验、性能和限流逐层推进。

## Frame 3

- status: outline
- src: compositions/03-request.html
- rules: `svg-path-draw`, `center-outward-expansion`
- beat: 复盘一次真实请求：curl 进入 HTTP handler，经 Provider 到 Mock，再把 JSON 或 SSE 返回；Mock 文本固定，不代表模型理解。

## Frame 4

- status: outline
- src: compositions/04-routing.html
- rules: `svg-path-draw`, `stat-bars-and-fills`
- beat: 客户端选择 economy / balanced / powerful 逻辑档位，配置映射候选组；组内按占用比例选择，组间在有限错误条件下 fallback。

## Frame 5

- status: outline
- src: compositions/05-streaming.html
- rules: `svg-path-draw`, `center-outward-expansion`
- beat: SSE 把完整答案拆成事件；收到响应头不等于结束，客户端断连通过 context 取消，上游 Body.Close 才完成资源归还。

## Frame 6

- status: outline
- src: compositions/06-capacity.html
- rules: `stat-bars-and-fills`, `svg-path-draw`
- beat: 容量名额像有限座位；满载返回 503 而不是塞进无界内存队列。慢客户端形成背压，网关不会无限缓存上游内容。

## Frame 7

- status: outline
- src: compositions/07-jobs.html
- rules: `svg-path-draw`, `stat-bars-and-fills`
- beat: 任务先提交到 SQLite，202 只确认任务已入库；worker 独立执行。崩溃恢复可能重做供应商调用，因此语义不是 exactly-once。

## Frame 8

- status: outline
- src: compositions/08-rate-limit.html
- rules: `stat-bars-and-fills`, `svg-path-draw`
- beat: 令牌桶限制新请求到达速度，容量名额限制尚未结束的工作，两者分别处理速率和在途资源。

## Frame 9

- status: outline
- src: compositions/09-next-steps.html
- rules: `center-outward-expansion`, `svg-path-draw`
- beat: 使用三个本地 Mock 和一个网关启动整套实验，然后从第一课读到并发与任务模块；收束为“沿着请求追踪所有权”。
