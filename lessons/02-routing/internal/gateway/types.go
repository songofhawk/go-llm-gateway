// Package gateway 实现与业务无关的 LLM 路由。HTTP 层调用此包。
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

var (
	ErrInvalid  = errors.New("invalid request")
	ErrUpstream = errors.New("upstream failed")
	ErrTooLarge = errors.New("upstream response exceeds limit")
)

// Request 保留工具调用、多模态等原始 JSON 字段，只解析网关必须理解的字段。
// model 是逻辑档次 economy / balanced / powerful，真正的模型 ID 由配置决定。
type Request struct {
	Tier   string
	Stream bool
	Fields map[string]json.RawMessage
}

func ParseRequest(data []byte) (Request, error) {
	var r Request
	if err := json.Unmarshal(data, &r.Fields); err != nil || r.Fields == nil {
		return r, fmt.Errorf("%w: expected JSON object", ErrInvalid)
	}
	r.Tier = "balanced"
	if v, ok := r.Fields["model"]; ok {
		if bytes.Equal(v, []byte("null")) {
			return r, fmt.Errorf("%w: model must be a tier", ErrInvalid)
		}
		if err := json.Unmarshal(v, &r.Tier); err != nil {
			return r, fmt.Errorf("%w: model must be a tier", ErrInvalid)
		}
	}
	if r.Tier != "economy" && r.Tier != "balanced" && r.Tier != "powerful" {
		return r, fmt.Errorf("%w: unknown model tier", ErrInvalid)
	}
	if v, ok := r.Fields["stream"]; ok {
		if bytes.Equal(v, []byte("null")) {
			return r, fmt.Errorf("%w: stream must be boolean", ErrInvalid)
		}
		if err := json.Unmarshal(v, &r.Stream); err != nil {
			return r, fmt.Errorf("%w: stream must be boolean", ErrInvalid)
		}
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(r.Fields["messages"], &messages); err != nil || len(messages) == 0 {
		return r, fmt.Errorf("%w: messages must be a nonempty array", ErrInvalid)
	}
	return r, nil
}

// Provider 是协议适配器：负责请求编码、认证和发送，不决定模型路由。
// 返回的 Body 归调用者所有。必须 Close，即使读取失败或客户端已经断开。
type Provider interface {
	Open(context.Context, string, Request) (io.ReadCloser, error)
}

// UpstreamError 只携带可公开的状态码，不透传供应商响应正文或含密钥的 URL。
type UpstreamError struct{ Status int }

func (e *UpstreamError) Error() string { return fmt.Sprintf("upstream HTTP %d", e.Status) }

// Target 将供应商实例和实际模型绑定；一组 Target 负载均衡，组间按序 fallback。
type Target struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}
type Endpoint struct {
	Name      string `json:"name"`
	BaseURL   string `json:"base_url"`
	APIKeyEnv string `json:"api_key_env"`
}
type Config struct {
	Endpoints []Endpoint            `json:"endpoints"`
	Routes    map[string][][]Target `json:"routes"`
}

// 总调用、单候选、响应头时限确保 fallback 有界；读写空闲时限在第 3 课加入。
type Timeouts struct{ Total, Attempt, Header time.Duration }

func DefaultTimeouts() Timeouts {
	return Timeouts{Total: 10 * time.Minute, Attempt: 4 * time.Minute, Header: 30 * time.Second}
}
