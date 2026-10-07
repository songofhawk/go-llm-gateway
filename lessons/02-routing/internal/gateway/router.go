package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

const MaxResponseBytes = 8 << 20 // 一次性结果有上限；流式响应不全量缓冲。

type backend struct{ provider Provider }

// 第 2 课先用轮转选择候选。锁仅保护轮转游标，不覆盖网络 I/O。
// 按占用比例选择、并发容量和名额释放会在第 3 课加入。
type Gateway struct {
	mu       sync.Mutex
	backends map[string]*backend
	routes   map[string][][]Target
	cursor   uint64
	times    Timeouts
}

func New(config Config, providers map[string]Provider, times Timeouts) (*Gateway, error) {
	if times.Total <= 0 || times.Attempt <= 0 || times.Header <= 0 {
		return nil, errors.New("timeouts must be positive")
	}
	g := &Gateway{backends: map[string]*backend{}, routes: map[string][][]Target{}, times: times}
	for _, e := range config.Endpoints {
		if e.Name == "" || providers[e.Name] == nil {
			return nil, errors.New("invalid endpoint")
		}
		if _, exists := g.backends[e.Name]; exists {
			return nil, errors.New("duplicate endpoint")
		}
		g.backends[e.Name] = &backend{provider: providers[e.Name]}
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

// choose 在同组内轮转，每个候选在一次调用中最多尝试一次。
func (g *Gateway) choose(group []Target, tried map[Target]bool) (Target, *backend, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	start := int(g.cursor % uint64(len(group)))
	for i := 0; i < len(group); i++ {
		target := group[(start+i)%len(group)]
		if !tried[target] {
			g.cursor++
			return target, g.backends[target.Provider], true
		}
	}
	return Target{}, nil, false
}

// Result 的调用者拥有响应体，必须 Close 来关闭连接并取消请求计时器。
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
			target, b, ok := g.choose(group, tried)
			if !ok {
				break
			}
			tried[target] = true
			attempt, cancelAttempt := context.WithTimeout(overall, g.times.Attempt)
			raw, err := b.provider.Open(attempt, target.Model, r)
			if err == nil {
				body := raw
				if r.Stream {
					// 先读一小块，再把所有权交给调用者；空流和首块前断连允许 fallback。
					buf := make([]byte, 4096)
					var n int
					n, err = body.Read(buf)
					if n > 0 {
						prefix := io.MultiReader(bytes.NewReader(buf[:n]), body)
						handedOff = true
						return &Result{Body: &ownedBody{ReadCloser: &readerCloser{Reader: prefix, Closer: body}, cleanup: func() { cancelAttempt(); cancelAll() }}, Provider: target.Provider, Model: target.Model}, nil
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
						cancelAll()
						return &Result{Body: io.NopCloser(bytes.NewReader(data)), Provider: target.Provider, Model: target.Model}, nil
					}
				}
				body.Close()
			}
			// 在主动取消之前判断是否耗尽单候选时限。
			if attempt.Err() != nil && overall.Err() == nil {
				err = context.DeadlineExceeded
			}
			cancelAttempt()
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
	return nil, ErrUpstream
}

type readerCloser struct {
	io.Reader
	io.Closer
}
