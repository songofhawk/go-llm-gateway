// gateway 是完整教学入口。从 lessons/01-proxy 开始，再阅读本文件的依赖组装。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"example.com/llm-gateway/internal/gateway"
	"example.com/llm-gateway/internal/jobs"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
func run() error {
	debugAddr := flag.String("pprof", "", "可选的独立本机 pprof 地址，例如 localhost:6060")
	configPath := flag.String("config", "config.example.json", "provider 和路由配置")
	address := flag.String("addr", "localhost:8080", "监听地址")
	dbPath := flag.String("db", "gateway.db", "单进程 SQLite 数据库")
	workers := flag.Int("workers", 4, "后台调用并发数")
	queueSize := flag.Int("queue", 128, "未完成任务容量")
	streams := flag.Int("streams", 64, "前台流式并发数")
	unary := flag.Int("unary", 64, "前台一次性并发数")
	rate := flag.Float64("rate", 0, "全局新请求速率（次/秒）；0 配合 burst=0 关闭")
	burst := flag.Int("burst", 0, "令牌桶可积累的突发请求数；开启限流时至少 1")
	times := gateway.DefaultTimeouts()
	flag.DurationVar(&times.Total, "total-timeout", times.Total, "单调用总时限")
	flag.DurationVar(&times.Attempt, "attempt-timeout", times.Attempt, "单候选总时限")
	flag.DurationVar(&times.Idle, "idle-timeout", times.Idle, "上游两次读取间最长等待")
	flag.DurationVar(&times.Header, "header-timeout", times.Header, "等待上游响应头时限")
	flag.DurationVar(&times.Write, "write-timeout", times.Write, "下游单次写入时限")
	flag.Parse()
	if *workers < 1 || *queueSize < 1 || *streams < 1 || *unary < 1 {
		return errors.New("capacities must be positive")
	}
	limiter, err := gateway.NewRateLimiter(*rate, *burst)
	if err != nil {
		return err
	}
	token := os.Getenv("GATEWAY_TOKEN")
	host, _, err := net.SplitHostPort(*address)
	if err != nil {
		return errors.New("invalid listen address")
	}
	ip := net.ParseIP(host)
	if token == "" && host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("non-loopback listen requires GATEWAY_TOKEN")
	}
	file, err := os.Open(*configPath)
	if err != nil {
		return fmt.Errorf("open config: %w", err)
	}
	var config gateway.Config
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	err = decoder.Decode(&config)
	file.Close()
	if err != nil {
		return errors.New("invalid config JSON")
	}
	client := gateway.NewHTTPClient(times.Header)
	defer client.CloseIdleConnections()
	providers := map[string]gateway.Provider{}
	for _, endpoint := range config.Endpoints {
		if err := gateway.ValidateBaseURL(endpoint.BaseURL); err != nil {
			return err
		}
		key := ""
		if endpoint.APIKeyEnv != "" {
			key = os.Getenv(endpoint.APIKeyEnv)
			if strings.TrimSpace(key) == "" {
				return fmt.Errorf("missing provider credential env: %s", endpoint.APIKeyEnv)
			}
		}
		providers[endpoint.Name] = &gateway.OpenAIProvider{URL: endpoint.BaseURL, Key: key, Client: client}
	}
	g, err := gateway.New(config, providers, times)
	if err != nil {
		return err
	}
	closeDebug, debugDone, err := startDiagnostics(*debugAddr, g)
	if err != nil {
		return err
	}
	defer closeDebug()
	store, err := jobs.Open(*dbPath, *queueSize)
	if err != nil {
		return fmt.Errorf("open job store: %w", err)
	}
	defer store.Close()
	api := gateway.NewAPI(g, store, token, *streams, *unary)
	api.Limiter = limiter
	// 不用整个响应的 WriteTimeout 去截断长流；每次写由 ResponseController 控制。
	// BaseContext 允许关机宽限期结束后统一取消所有在途 HTTP 请求。
	requests, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()
	server := &http.Server{Addr: *address, Handler: api.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10,
		BaseContext: func(net.Listener) context.Context { return requests },
	}
	listener, err := net.Listen("tcp", *address)
	if err != nil {
		return err
	}
	defer listener.Close()
	workerCtx, cancelWorkers := context.WithCancel(context.Background())
	defer cancelWorkers()
	workerDone := make(chan error, 1)
	go func() { workerDone <- store.Run(workerCtx, *workers, g.ExecuteJob) }()
	httpDone := make(chan error, 1)
	go func() { httpDone <- server.Serve(listener) }()
	signals, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("gateway listening on %s; workers=%d streams=%d unary=%d", *address, *workers, *streams, *unary)
	var runErr error
	workerExited := false
	select {
	case <-signals.Done():
	case err := <-debugDone:
		runErr = fmt.Errorf("pprof server stopped: %w", err)
	case err := <-workerDone:
		workerExited = true
		runErr = fmt.Errorf("job workers stopped: %w", err)
	case err := <-httpDone:
		if !errors.Is(err, http.ErrServerClosed) {
			runErr = err
		}
	}
	// 停止接收请求，给前台调用短暂完成机会；后台立即取消、保留可恢复状态。
	cancelWorkers()
	shutdown, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	if err := server.Shutdown(shutdown); err != nil {
		cancelRequests()
		_ = server.Close()
	}
	cancelRequests()
	if !workerExited {
		if err := <-workerDone; err != nil && !errors.Is(err, context.Canceled) && runErr == nil {
			runErr = err
		}
	}
	return runErr
}
