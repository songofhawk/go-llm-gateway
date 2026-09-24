package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// OpenAIProvider 支持 Chat Completions 兼容服务，不等于所有厂商的原生协议。
// 同一实例并发安全；Transport 跨请求复用连接，而不是每次 new Client。
type OpenAIProvider struct {
	URL    string
	Key    string
	Client *http.Client
}

func NewHTTPClient(headerTimeout time.Duration) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	tr.MaxIdleConns = 256
	tr.MaxIdleConnsPerHost = 64
	tr.IdleConnTimeout = 90 * time.Second
	tr.TLSHandshakeTimeout = 5 * time.Second
	tr.ResponseHeaderTimeout = headerTimeout
	// 不设置 Client.Timeout=10s：它涵盖完整 body，会把正常长流截断。
	// 活跃请求数由网关容量控制；HTTP/2 多路复用时连接数不等于请求数。
	return &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func ValidateBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("invalid provider base_url")
	}
	return nil
}

func (p *OpenAIProvider) Open(ctx context.Context, model string, r Request) (io.ReadCloser, error) {
	// 复制 map，避免并发修改用户请求或污染下一个 fallback 的模型参数。
	fields := make(map[string]json.RawMessage, len(r.Fields)+1)
	for k, v := range r.Fields {
		fields[k] = v
	}
	fields["model"], _ = json.Marshal(model)
	body, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.URL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	if r.Stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	if p.Key != "" {
		req.Header.Set("Authorization", "Bearer "+p.Key)
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		// url.Error 会含内部地址；API 只返回通用错误，context 语义仍由调用链保留。
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		// 小错误响应有界排空，可复用连接。直接 Close 在高频 503/fallback 下
		// 会制造重连风暴；同时用时间和字节上限防止恶意错误体拖住请求。
		drainErrorBody(resp.Body)
		return nil, &UpstreamError{Status: resp.StatusCode}
	}
	media, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if (r.Stream && media != "text/event-stream") || (!r.Stream && media != "application/json") {
		resp.Body.Close()
		return nil, fmt.Errorf("%w: unexpected content type", ErrUpstream)
	}
	return resp.Body, nil
}

// drainErrorBody 只等待 100ms、最多读取 4KiB；超时关闭 HTTP body 会解除阻塞。
// 成功读到 EOF 时 Transport 可复用连接，超限则主动放弃该连接。
func drainErrorBody(body io.ReadCloser) {
	timer := time.AfterFunc(100*time.Millisecond, func() { _ = body.Close() })
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 4<<10))
	timer.Stop()
	_ = body.Close()
}
