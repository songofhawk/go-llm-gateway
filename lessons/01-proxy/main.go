// 包 main 实现第一课：一个固定的 OpenAI 兼容代理。
// 本示例刻意不包含模型路由、备用模型或容量管理，也没有达到生产安全要求；
// 请把它作为一个小型学习示例。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	chatPath       = "/v1/chat/completions"
	maxBodyBytes   = 1 << 20 // 1 MiB（兆字节）
	requestTimeout = 2 * time.Minute
	readTimeout    = 15 * time.Second
)

type gateway struct {
	proxy http.Handler
}

func newGateway(upstreamBase *url.URL, apiKey string, transport http.RoundTripper) *gateway {
	proxy := httputil.NewSingleHostReverseProxy(upstreamBase)
	proxy.Transport = transport

	// 配置的上游 URL 以 /v1 结尾。把收到的公开路径替换为固定的
	// /chat/completions，调用方因此不能选择其他上游路径。
	proxy.Director = func(req *http.Request) {
		req.URL.Scheme = upstreamBase.Scheme
		req.URL.Host = upstreamBase.Host
		req.URL.Path = strings.TrimRight(upstreamBase.Path, "/") + "/chat/completions"
		req.URL.RawPath = ""
		req.Host = upstreamBase.Host
		req.Header.Del("Authorization") // 不转发调用方提供的认证凭据。
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
	}

	// 不允许上游把代理重定向到其他主机或路径。
	proxy.ModifyResponse = func(resp *http.Response) error {
		if resp.StatusCode >= http.StatusMultipleChoices && resp.StatusCode < http.StatusBadRequest {
			return errors.New("upstream redirect rejected")
		}
		return nil
	}
	proxy.FlushInterval = -1 // 上游每次写入后立即刷新，及时发送 SSE 数据。
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		if errors.Is(r.Context().Err(), context.Canceled) {
			return // 调用方已断开；请求 context 会取消上游请求。
		}
		status := http.StatusBadGateway
		if errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		}
		writeError(w, status, "upstream request failed")
	}

	return &gateway{proxy: proxy}
}

func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != chatPath {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if r.URL.RawQuery != "" {
		writeError(w, http.StatusBadRequest, "query parameters are not supported")
		return
	}

	// 服务端的 ReadTimeout 限制客户端发送请求体所用的时间，MaxBytesReader
	// 限制读取的字节数和内存用量。开始代理前，再设置总请求时限并继承入站请求的 context。
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "request body could not be read")
		return
	}
	if len(body) == 0 {
		writeError(w, http.StatusBadRequest, "request body is required")
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	g.proxy.ServeHTTP(w, r.WithContext(ctx))
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func upstreamURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("invalid UPSTREAM_URL")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || !strings.HasSuffix(u.Path, "/v1") {
		return nil, errors.New("UPSTREAM_URL must be an http(s) URL ending in /v1")
	}
	return u, nil
}

func main() {
	base, err := upstreamURL(os.Getenv("UPSTREAM_URL"))
	if err != nil {
		log.Fatal(err)
	}
	apiKey := os.Getenv("UPSTREAM_API_KEY") // 密钥只保留在内存中，绝不写入日志。
	if apiKey == "" {
		log.Fatal("UPSTREAM_API_KEY is required")
	}
	addr := os.Getenv("GATEWAY_ADDR")
	if addr == "" {
		addr = "localhost:8081"
	}

	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	defer transport.CloseIdleConnections()
	server := &http.Server{
		Addr:              addr,
		Handler:           newGateway(base, apiKey, transport),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       readTimeout,
		// 此处不设置 WriteTimeout：固定写入时限可能截断正常的长模型响应流。
		// 生产部署需要更细致地限制写入时长。
	}
	log.Printf("gateway listening at %s", addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
