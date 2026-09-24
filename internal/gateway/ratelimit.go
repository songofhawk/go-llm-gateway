package gateway

import (
	"errors"
	"math"
	"sync"
	"time"
)

// RateLimiter 是单进程、全局共享的请求令牌桶，与“同时在途”的并发名额不同。
// rate 为每秒补充的通行令牌，burst 为最多积累的令牌；一次新请求消耗一个。
// 不启动定时协程：请求到来时按经过的时间补充，空闲时没有后台维护成本。
// 这里的令牌不是 LLM 输入/输出 token，也不代表供应商费用配额。
type RateLimiter struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
}

// NewRateLimiter 的零值配置 (0,0) 返回 nil，表示关闭。
// 非零时两项都必须有效；明确限制极端速率，保证 Retry-After 可安全表示。
func NewRateLimiter(rate float64, burst int) (*RateLimiter, error) {
	if rate == 0 && burst == 0 {
		return nil, nil
	}
	if math.IsNaN(rate) || math.IsInf(rate, 0) || rate < 0.001 || rate > 1e6 || burst < 1 || burst > 1e6 {
		return nil, errors.New("rate must be 0.001..1000000 requests/s and burst 1..1000000; use both zero to disable")
	}
	return &RateLimiter{rate: rate, burst: float64(burst), tokens: float64(burst), last: time.Now()}, nil
}

// Allow 立即判断，不排队、不睡眠。拒绝返回至少 1 秒的 Retry-After 建议值。
// 多客户端共享桶，等待该时长不保证下次一定成功；其他请求可能先消耗令牌。
func (l *RateLimiter) Allow() (bool, int) {
	if l == nil {
		return true, 0
	}
	return l.allowAt(time.Now())
}

// 时间作为参数方便测试边界；实际调用 time.Now 带有单调时钟信息。
func (l *RateLimiter) allowAt(now time.Time) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.After(l.last) {
		l.tokens = math.Min(l.burst, l.tokens+now.Sub(l.last).Seconds()*l.rate)
		l.last = now
	}
	if l.tokens >= 1 {
		l.tokens--
		return true, 0
	}
	return false, max(1, int(math.Ceil((1-l.tokens)/l.rate)))
}
