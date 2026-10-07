# 压测结果摘录

这些小文件支撑 `../STRESS-REPORT.md` 中展示的结果。内容来自本机 Mock 压测，不含真实提示词、供应商密钥、数据库或编译二进制。`scale-10k/result.json` 记录 10,000 worker 的闭环过载测试；`ceiling/` 记录调高容量后的本机成功吞吐阶梯和负载端资源拐点。这些结果不代表生产容量。

每个目录的 `result.json` 是该场景完整的请求统计；`*-stats.json` 是网关的运行时快照；`*-mock-*.json` 是模拟供应商请求计数；`*-top.txt` 是对应 pprof profile 的函数摘要。`block-top.txt` 和 `mutex-top.txt` 中的等待时间为所有 goroutine 的累计时间，不等于单个请求耗时。

2026-10-07 新增 `cancel-http1-20261007`、`cancel-h2c-ipv4-20261007`、`cancel-h2c-ipv6-20261007` 三个隔离的 64 并发、60 秒取消对照。HTTP/1 失败结果保留了负载中的 TCP 状态聚合和只读网络配置；两轮 h2c 没有非预期错误，取消均到达上游。新增 JSON 字段统计实际 HTTP 协议、客户端建连和 Mock 接受连接数。旧的 `cancel` / `cancel-ipv6` 目录仍是原始 HTTP/1 历史失败。

完整 profile 与当轮网关二进制会使仓库大幅增长，未放入版本控制。完整压测可运行 `bash ../../scripts/stress.sh` 重新生成。高并发重跑可设置 `SCENARIOS=unary PROFILE=0 UNARY_CONCURRENCY=10000 STRESS_DURATION=12s`；当前 cancel 默认使用 h2c，`HTTP_PROTOCOL=http1` 可运行旧协议对照。
