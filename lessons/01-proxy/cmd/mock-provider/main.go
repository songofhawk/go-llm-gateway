// mock-provider 在本机模拟 LLM 的生成时长、分块、失败和停流，不调用任何模型。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

type settings struct {
	status, chunks, chunkBytes, failEvery, failureStatus, stallAfter int
	delay                                                            time.Duration
	truncate                                                         bool
	connections                                                      *atomic.Int64
}
type counters struct{ requests, active, peak, completed, canceled, faults atomic.Int64 }

func newMock(cfg settings) http.Handler {
	stats := &counters{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") })
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var connections int64
		if cfg.connections != nil {
			connections = cfg.connections.Load()
		}
		_ = json.NewEncoder(w).Encode(map[string]int64{"requests": stats.requests.Load(), "active": stats.active.Load(), "peak": stats.peak.Load(), "completed": stats.completed.Load(), "canceled": stats.canceled.Load(), "faults": stats.faults.Load(), "connections": connections})
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var req struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", 400)
			return
		}
		number := stats.requests.Add(1)
		active := stats.active.Add(1)
		defer stats.active.Add(-1)
		for {
			old := stats.peak.Load()
			if old >= active || stats.peak.CompareAndSwap(old, active) {
				break
			}
		}
		completed := false
		defer func() {
			if completed {
				stats.completed.Add(1)
			} else if r.Context().Err() != nil {
				stats.canceled.Add(1)
			}
		}()
		status := cfg.status
		if cfg.failEvery > 0 && number%int64(cfg.failEvery) == 0 {
			status = cfg.failureStatus
		}
		if status != 200 {
			stats.faults.Add(1)
			http.Error(w, "simulated failure", status)
			return
		}
		if !req.Stream {
			// 一次性响应也模拟完整的生成过程，只是等所有生成间隔结束后
			// 才把完整答案交给客户端。这样可以和流式模式比较首字节时间。
			for i := 0; i < cfg.chunks; i++ {
				if !wait(r.Context(), cfg.delay) {
					return
				}
			}
			var content strings.Builder
			for i := 0; i < cfg.chunks; i++ {
				content.WriteString(mockChunk(i, cfg.chunkBytes))
			}
			w.Header().Set("Content-Type", "application/json")
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Second))
			err := json.NewEncoder(w).Encode(map[string]any{"id": "mock", "object": "chat.completion", "model": req.Model, "choices": []any{map[string]any{"index": 0, "message": map[string]string{"role": "assistant", "content": content.String()}, "finish_reason": "stop"}}})
			completed = err == nil
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		rc := http.NewResponseController(w)
		for i := 0; i < cfg.chunks; i++ {
			if cfg.stallAfter == i {
				_ = rc.SetWriteDeadline(time.Now().Add(5 * time.Second))
				if rc.Flush() != nil {
					return
				}
				<-r.Context().Done()
				return
			}
			if !wait(r.Context(), cfg.delay) {
				return
			}
			data, _ := json.Marshal(map[string]any{"id": "mock", "object": "chat.completion.chunk", "model": req.Model, "choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": mockChunk(i, cfg.chunkBytes)}}}})
			if rc.SetWriteDeadline(time.Now().Add(5*time.Second)) != nil {
				return
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
				return
			}
			if rc.Flush() != nil {
				return
			}
		}
		if !cfg.truncate {
			_, err := fmt.Fprint(w, "data: [DONE]\n\n")
			completed = err == nil
		}
	})
	return mux
}

// mockChunk 给每个块加上可见编号；一次性响应会把这些块拼接成同一段完整文本。
func mockChunk(index, chunkBytes int) string {
	label := fmt.Sprintf("block%d", index+1)
	if len(label) >= chunkBytes {
		return label[:chunkBytes]
	}
	return label + strings.Repeat(".", chunkBytes-len(label))
}

func wait(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
func main() {
	addr := flag.String("addr", "localhost:9090", "监听地址")
	cfg := settings{}
	flag.IntVar(&cfg.status, "status", 200, "固定上游状态码")
	flag.DurationVar(&cfg.delay, "delay", 300*time.Millisecond, "每个模拟生成步骤的间隔")
	flag.IntVar(&cfg.chunks, "chunks", 5, "模拟生成步骤数；一次性响应等完全部步骤，流式逐步发送")
	flag.IntVar(&cfg.chunkBytes, "chunk-bytes", 64, "每个内容块的字节数")
	flag.IntVar(&cfg.failEvery, "fail-every", 0, "每 N 次请求失败一次；0 关闭")
	flag.IntVar(&cfg.failureStatus, "failure-status", 503, "周期性故障的 HTTP 状态")
	flag.IntVar(&cfg.stallAfter, "stall-after", -1, "流式发出 N 块后停住直到取消；-1 关闭")
	flag.BoolVar(&cfg.truncate, "truncate", false, "省略 DONE，模拟截断")
	flag.Parse()
	if cfg.delay < 0 || cfg.chunks < 1 || cfg.chunkBytes < 1 || cfg.chunkBytes > 1<<20 || cfg.failEvery < 0 || cfg.stallAfter < -1 || cfg.status < 200 || cfg.status > 599 || cfg.failureStatus < 400 || cfg.failureStatus > 599 {
		log.Fatal("invalid mock settings")
	}
	server := newMockServer(*addr, cfg)
	log.Printf("mock on %s; chunks=%d delay=%s", *addr, cfg.chunks, cfg.delay)
	log.Fatal(server.ListenAndServe())
}

func newMockServer(addr string, cfg settings) *http.Server {
	connections := new(atomic.Int64)
	cfg.connections = connections
	server := &http.Server{Addr: addr, Handler: newMock(cfg), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		ConnState: func(_ net.Conn, state http.ConnState) {
			if state == http.StateNew {
				connections.Add(1)
			}
		},
	}
	return server
}
