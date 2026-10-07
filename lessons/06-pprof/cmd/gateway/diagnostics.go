package main

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/pprof"
	"runtime"
	"time"

	"example.com/llm-gateway/internal/gateway"
)

// startDiagnostics 默认关闭，启用后使用独立、只绑定本机的端口。
// 显式使用自己的 mux，避免把 pprof 顺手挂到业务 HTTP 入口。
func startDiagnostics(addr string, g *gateway.Gateway) (func(), <-chan error, error) {
	if addr == "" {
		return func() {}, nil, nil
	}
	if err := validateDebugAddress(addr); err != nil {
		return nil, nil, err
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	if !listener.Addr().(*net.TCPAddr).IP.IsLoopback() {
		listener.Close()
		return nil, nil, errors.New("pprof must listen on loopback")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
	mux.HandleFunc("GET /debug/stats", func(w http.ResponseWriter, r *http.Request) {
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"time": time.Now().UTC(), "goroutines": runtime.NumGoroutine(), "heap_alloc_bytes": mem.HeapAlloc, "heap_inuse_bytes": mem.HeapInuse, "heap_objects": mem.HeapObjects, "num_gc": mem.NumGC, "providers": g.Snapshot()})
	})
	// 阻塞记录按约 1ms 采样，锁竞争记录约每 5 次采一次；仅在显式启用时付出成本。
	runtime.SetBlockProfileRate(1_000_000)
	previous := runtime.SetMutexProfileFraction(5)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	close := func() { _ = server.Close(); runtime.SetBlockProfileRate(0); runtime.SetMutexProfileFraction(previous) }
	return close, done, nil
}
func validateDebugAddress(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return errors.New("invalid pprof address")
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("pprof address must use localhost or loopback IP")
	}
	return nil
}
