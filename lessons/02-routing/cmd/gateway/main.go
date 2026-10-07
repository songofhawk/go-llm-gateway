// gateway 是第 2 课的独立入口：只组装本课目录内的模块。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"example.com/llm-gateway/internal/gateway"
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
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
func run() error {
	configPath := flag.String("config", "config.example.json", "provider 和路由配置")
	address := flag.String("addr", "localhost:8080", "监听地址")
	times := gateway.DefaultTimeouts()
	flag.DurationVar(&times.Total, "total-timeout", times.Total, "单调用总时限")
	flag.DurationVar(&times.Attempt, "attempt-timeout", times.Attempt, "单候选总时限")
	flag.DurationVar(&times.Header, "header-timeout", times.Header, "等待上游响应头时限")
	flag.Parse()
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
	client := gateway.NewHTTPClient(times.Header, 100)
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
	api := gateway.NewAPI(g, token)
	// 不设置短的全响应 WriteTimeout，避免截断长流；细粒度写时限留到第 3 课。
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
	httpDone := make(chan error, 1)
	go func() { httpDone <- server.Serve(listener) }()
	signals, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("gateway listening on %s", *address)
	var runErr error
	select {
	case <-signals.Done():
	case err := <-httpDone:
		if !errors.Is(err, http.ErrServerClosed) {
			runErr = err
		}
	}
	// 停止接收新请求，等待前台调用；宽限期结束后取消剩余请求。
	shutdown, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	if err := server.Shutdown(shutdown); err != nil {
		cancelRequests()
		_ = server.Close()
	}
	cancelRequests()
	return runErr
}
