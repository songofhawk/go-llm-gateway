package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

const MaxResponseBytes = 8 << 20 // 一次性结果有上限；流式响应不全量缓冲。

type backend struct {
	provider               Provider
	active, capacity, peak int
}

// Gateway 的锁只保护短小的计数修改，绝不跨网络调用持有锁。
// provider 在所有模型路由间共用容量，防止 economy/powerful 各自超卖同一端点。
type Gateway struct {
	mu       sync.Mutex
	backends map[string]*backend
	routes   map[string][][]Target
	cursor   uint64
	times    Timeouts
}

func New(config Config, providers map[string]Provider, times Timeouts) (*Gateway, error) {
	if times.Total <= 0 || times.Attempt <= 0 || times.Idle <= 0 || times.Header <= 0 || times.Write <= 0 {
		return nil, errors.New("timeouts must be positive")
	}
	g := &Gateway{backends: map[string]*backend{}, routes: map[string][][]Target{}, times: times}
	for _, e := range config.Endpoints {
		if e.Name == "" || e.Capacity <= 0 || providers[e.Name] == nil {
			return nil, errors.New("invalid endpoint")
		}
		if _, exists := g.backends[e.Name]; exists {
			return nil, errors.New("duplicate endpoint")
		}
		g.backends[e.Name] = &backend{provider: providers[e.Name], capacity: e.Capacity}
	}
	for _, tier := range []string{"economy", "balanced", "powerful"} {
		groups := config.Routes[tier]
		if len(groups) == 0 {
			return nil, fmt.Errorf("missing route: %s", tier)
		}
		seen := map[Target]bool{}
		for _, group := range groups {
			if len(group) == 0 {
				return nil, errors.New("empty route group")
			}
			for _, t := range group {
				if g.backends[t.Provider] == nil || t.Model == "" || seen[t] {
					return nil, errors.New("invalid or duplicate route target")
				}
				seen[t] = true
			}
			g.routes[tier] = append(g.routes[tier], append([]Target(nil), group...))
		}
		if len(seen) > 16 {
			return nil, errors.New("at most 16 targets per tier")
		}
	}
	return g, nil
}

// acquire 选择占用比例最低的端点；并列时轮转，选择与占位在同一临界区。
func (g *Gateway) acquire(group []Target, tried map[Target]bool) (Target, *backend, func(), bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var chosen Target
	var selected *backend
	start := int(g.cursor % uint64(len(group)))
	for i := 0; i < len(group); i++ {
		t := group[(start+i)%len(group)]
		b := g.backends[t.Provider]
		if tried[t] || b.active >= b.capacity {
			continue
		}
		if selected == nil || float64(b.active)/float64(b.capacity) < float64(selected.active)/float64(selected.capacity) {
			chosen, selected = t, b
		}
	}
	if selected == nil {
		return Target{}, nil, nil, false
	}
	selected.active++
	if selected.active > selected.peak {
		selected.peak = selected.active
	}
	g.cursor++
	var once sync.Once
	release := func() { once.Do(func() { g.mu.Lock(); selected.active--; g.mu.Unlock() }) }
	return chosen, selected, release, true
}

// Result 的 Close 是资源所有权边界：关闭 body、取消计时器、释放上游名额。
// 无论同步 handler 或异步 worker，都应立即 defer result.Body.Close()。
type Result struct {
	Body            io.ReadCloser
	Provider, Model string
}
type ownedBody struct {
	io.ReadCloser
	once    sync.Once
	cleanup func()
}

func (b *ownedBody) Close() error {
	var err error
	b.once.Do(func() { err = b.ReadCloser.Close(); b.cleanup() })
	return err
}

// idleBody 每次阻塞读取有独立计时器；超时取消 HTTP request，使 Read 真正退出。
// 不启动“每次 Read 一个 goroutine”，避免无法退出的后台读协程。
type idleBody struct {
	io.ReadCloser
	timeout time.Duration
	cancel  context.CancelFunc
}

func (b *idleBody) Read(p []byte) (int, error) {
	timer := time.AfterFunc(b.timeout, b.cancel)
	n, err := b.ReadCloser.Read(p)
	timer.Stop()
	return n, err
}

func retryable(err error) bool {
	var status *UpstreamError
	if errors.As(err, &status) {
		return status.Status == 408 || status.Status == 429 || status.Status >= 500
	}
	return true // 网络、读取、协议错误可以在尚未输出时换候选。
}

func (g *Gateway) Do(ctx context.Context, r Request) (*Result, error) {
	groups, ok := g.routes[r.Tier]
	if !ok {
		return nil, ErrInvalid
	}
	overall, cancelAll := context.WithTimeout(ctx, g.times.Total)
	handedOff := false
	defer func() {
		if !handedOff {
			cancelAll()
		}
	}()
	tried := map[Target]bool{}
	var last error
	for _, group := range groups {
		for {
			if err := overall.Err(); err != nil {
				return nil, err
			}
			target, b, release, ok := g.acquire(group, tried)
			if !ok {
				break
			}
			tried[target] = true
			attempt, cancelAttempt := context.WithTimeout(overall, g.times.Attempt)
			raw, err := b.provider.Open(attempt, target.Model, r)
			if err == nil {
				body := &idleBody{ReadCloser: raw, timeout: g.times.Idle, cancel: cancelAttempt}
				if r.Stream {
					// 先读一小块，再把所有权交给调用者；空流和首块前断连允许 fallback。
					buf := make([]byte, 4096)
					var n int
					n, err = body.Read(buf)
					if n > 0 {
						prefix := io.MultiReader(bytes.NewReader(buf[:n]), body)
						handedOff = true
						return &Result{Body: &ownedBody{ReadCloser: &readerCloser{Reader: prefix, Closer: body}, cleanup: func() { cancelAttempt(); release(); cancelAll() }}, Provider: target.Provider, Model: target.Model}, nil
					}
					if err == nil {
						err = io.ErrNoProgress
					}
				} else {
					var data []byte
					data, err = io.ReadAll(io.LimitReader(body, MaxResponseBytes+1))
					if err == nil && len(data) > MaxResponseBytes {
						err = ErrTooLarge
					}
					if err == nil && !json.Valid(data) {
						err = fmt.Errorf("%w: invalid JSON", ErrUpstream)
					}
					if err == nil {
						body.Close()
						cancelAttempt()
						release()
						cancelAll()
						return &Result{Body: io.NopCloser(bytes.NewReader(data)), Provider: target.Provider, Model: target.Model}, nil
					}
				}
				body.Close()
			}
			// 在主动 cleanup 之前判断：idleBody 的取消也代表超时。
			if attempt.Err() != nil && overall.Err() == nil {
				err = context.DeadlineExceeded
			}
			cancelAttempt()
			release()
			if overall.Err() != nil {
				return nil, overall.Err()
			}
			last = err
			if !retryable(err) {
				return nil, err
			}
		}
	}
	if last != nil {
		return nil, last
	}
	return nil, ErrBusy
}

type readerCloser struct {
	io.Reader
	io.Closer
}

// Snapshot 复制在途计数供压测观察；不暴露 provider URL、密钥或请求正文。
// pprof 显示协程在做什么，这个快照补充说明它们是否还占用业务名额。
func (g *Gateway) Snapshot() map[string]map[string]int {
	g.mu.Lock()
	defer g.mu.Unlock()
	result := make(map[string]map[string]int, len(g.backends))
	for name, b := range g.backends {
		result[name] = map[string]int{"active": b.active, "capacity": b.capacity, "peak": b.peak}
	}
	return result
}
