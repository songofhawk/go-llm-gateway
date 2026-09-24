# 压测结果摘录

这些小文件支撑 `../STRESS-REPORT.md` 中展示的结果。内容来自本机 Mock 压测，不含真实提示词、供应商密钥、数据库或编译二进制。

每个目录的 `result.json` 是该场景完整的请求统计；`*-stats.json` 是网关的运行时快照；`*-mock-*.json` 是模拟供应商请求计数；`*-top.txt` 是对应 pprof profile 的函数摘要。`block-top.txt` 和 `mutex-top.txt` 中的等待时间为所有 goroutine 的累计时间，不等于单个请求耗时。

完整 profile 与当轮网关二进制会使仓库大幅增长，未放入版本控制。完整压测可运行 `bash ../../scripts/stress.sh` 重新生成。高频取消的连接异常作为失败结果保留。
