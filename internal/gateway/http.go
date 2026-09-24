package gateway

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"example.com/llm-gateway/internal/jobs"
)

const MaxRequestBytes = 1 << 20

// API 将 HTTP 细节与模型执行分离。流式/一次性各自限制并发，后台使用固定 worker。
// channel 在这里是信号量，里面的空 struct 不存放任务，也不必关闭。
type API struct {
	Gateway        *Gateway
	Store          *jobs.Store
	Token          string
	Limiter        *RateLimiter // 在 Handler 开始服务前配置；nil 表示不限请求速率。
	streams, unary chan struct{}
	incoming       chan struct{} // 在读取 body 前占位，限制并发上传和任务查询的内存使用。
}

func NewAPI(g *Gateway, store *jobs.Store, token string, streams, unary int) *API {
	return &API{Gateway: g, Store: store, Token: token, streams: make(chan struct{}, streams), unary: make(chan struct{}, unary), incoming: make(chan struct{}, streams+unary+32)}
}
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("POST /v1/chat/completions", a.chat)
	if a.Store != nil {
		mux.HandleFunc("POST /v1/jobs", a.submit)
		mux.HandleFunc("GET /v1/jobs/{id}", a.getJob)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.Token != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+a.Token)) != 1 {
			writeError(w, 401, "unauthorized")
			return
		}
		// 鉴权后、读取正文前限制新调用速率。查询与健康检查不扣令牌。
		// fallback、SSE 分块及后台执行不再次扣入口令牌；失败也不退款。
		limitedRoute := r.Method == http.MethodPost && (r.URL.Path == "/v1/chat/completions" || (a.Store != nil && r.URL.Path == "/v1/jobs"))
		if limitedRoute {
			if allowed, retryAfter := a.Limiter.Allow(); !allowed {
				w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
				writeError(w, http.StatusTooManyRequests, "request rate limit exceeded")
				return
			}
		}
		if !acquireGate(a.incoming) {
			writeError(w, 503, "gateway at capacity")
			return
		}
		defer func() { <-a.incoming }()
		mux.ServeHTTP(w, r)
	})
}

func readRequest(w http.ResponseWriter, r *http.Request) ([]byte, Request, error) {
	// 限长度也限上传时间，防止慢请求永久占住 handler。
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return nil, Request{}, err
	}
	defer rc.SetReadDeadline(time.Time{})
	r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBytes)
	defer r.Body.Close()
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, Request{}, err
	}
	req, err := ParseRequest(data)
	return data, req, err
}
func acquireGate(gate chan struct{}) bool {
	select {
	case gate <- struct{}{}:
		return true
	default:
		return false
	}
}
func (a *API) chat(w http.ResponseWriter, r *http.Request) {
	_, req, err := readRequest(w, r)
	if err != nil {
		requestError(w, err)
		return
	}
	gate := a.unary
	if req.Stream {
		gate = a.streams
	}
	if !acquireGate(gate) {
		writeError(w, 503, "gateway at capacity")
		return
	}
	defer func() { <-gate }()
	result, err := a.Gateway.Do(r.Context(), req)
	if err != nil {
		executionError(w, err)
		return
	}
	defer result.Body.Close()
	w.Header().Set("X-Gateway-Provider", result.Provider)
	w.Header().Set("X-Gateway-Model", result.Model)
	rc := http.NewResponseController(w)
	defer rc.SetWriteDeadline(time.Time{})
	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		if err := relaySSE(w, result.Body, a.Gateway.times.Write); err != nil {
			// 此时 HTTP 200/部分 token 可能已经发送。中止响应，让客户端感知截断；
			// 不能再写 JSON 错误，也不能把 fallback 模型答案拼在后面。
			panic(http.ErrAbortHandler)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := rc.SetWriteDeadline(time.Now().Add(a.Gateway.times.Write)); err != nil {
		panic(http.ErrAbortHandler)
	}
	if _, err := io.Copy(w, result.Body); err != nil {
		panic(http.ErrAbortHandler)
	}
}

// doneDetector 只保留一行的前 32 字节。任意大的 SSE data 行仍然 O(1) 内存。
// [DONE] 必须独占 data 行且事件由空行结束，不能误识别模型正文中的该字符串。
type doneDetector struct {
	line                         []byte
	long, pending, done, afterCR bool
	dataLines                    int
}

// feed 返回当前块应转发的长度；终止事件后同一块里的额外字节不会泄漏。
// SSE 允许 LF、CRLF 和单独 CR；多个 data 行必须拼接，不能只检查最后一行。
func (d *doneDetector) feed(data []byte) int {
	for i, b := range data {
		if d.afterCR {
			d.afterCR = false
			if b == '\n' {
				continue
			}
		}
		if b == '\r' || b == '\n' {
			d.finishLine()
			if b == '\r' {
				d.afterCR = true
			}
			if d.done {
				end := i + 1
				if b == '\r' && end < len(data) && data[end] == '\n' {
					end++
				}
				return end
			}
		} else if len(d.line) < 32 {
			d.line = append(d.line, b)
		} else {
			d.long = true
		}
	}
	return len(data)
}
func (d *doneDetector) finishLine() {
	if !d.long && len(d.line) == 0 {
		d.done = d.pending && d.dataLines == 1
		d.pending = false
		d.dataLines = 0
	} else if bytes.HasPrefix(d.line, []byte("data:")) {
		d.dataLines++
		value := bytes.TrimPrefix(d.line, []byte("data:"))
		value = bytes.TrimPrefix(value, []byte(" "))
		d.pending = !d.long && d.dataLines == 1 && bytes.Equal(value, []byte("[DONE]"))
	}
	d.line = d.line[:0]
	d.long = false
}
func relaySSE(w http.ResponseWriter, body io.Reader, writeTimeout time.Duration) error {
	rc := http.NewResponseController(w)
	buf := make([]byte, 32<<10)
	detector := doneDetector{}
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if e := rc.SetWriteDeadline(time.Now().Add(writeTimeout)); e != nil {
				return e
			}
			forward := detector.feed(buf[:n])
			if _, e := w.Write(buf[:forward]); e != nil {
				return e
			}
			if e := rc.Flush(); e != nil {
				return e
			}
			if detector.done {
				return nil
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return io.ErrUnexpectedEOF
			}
			return err
		}
	}
}

func (a *API) submit(w http.ResponseWriter, r *http.Request) {
	data, req, err := readRequest(w, r)
	if err != nil {
		requestError(w, err)
		return
	}
	if req.Stream {
		writeError(w, 400, "background jobs require stream=false")
		return
	}
	job, err := a.Store.Submit(r.Context(), data)
	if errors.Is(err, jobs.ErrFull) {
		writeError(w, 503, "job queue at capacity")
		return
	}
	if err != nil {
		writeError(w, 503, "job storage unavailable")
		return
	}
	w.Header().Set("Location", "/v1/jobs/"+job.ID)
	writeJSON(w, http.StatusAccepted, job)
}
func (a *API) getJob(w http.ResponseWriter, r *http.Request) {
	job, err := a.Store.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, jobs.ErrNotFound) {
		writeError(w, 404, "job not found")
		return
	}
	if err != nil {
		writeError(w, 503, "job storage unavailable")
		return
	}
	writeJSON(w, 200, job)
}

// ExecuteJob 使用 worker 的 context，而不是提交 HTTP 请求的 context。
func (g *Gateway) ExecuteJob(ctx context.Context, payload json.RawMessage) (json.RawMessage, error) {
	req, err := ParseRequest(payload)
	if err != nil || req.Stream {
		return nil, ErrInvalid
	}
	result, err := g.Do(ctx, req)
	if err != nil {
		return nil, errors.New(publicExecutionError(err))
	}
	defer result.Body.Close()
	return io.ReadAll(result.Body)
}
func publicExecutionError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "upstream timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "request canceled"
	}
	if errors.Is(err, ErrBusy) {
		return "providers at capacity"
	}
	return "upstream failed"
}
func executionError(w http.ResponseWriter, err error) {
	status := 502
	var upstream *UpstreamError
	switch {
	case errors.Is(err, ErrInvalid):
		status = 400
	case errors.Is(err, context.DeadlineExceeded):
		status = 504
	case errors.Is(err, context.Canceled):
		status = 408
	case errors.Is(err, ErrBusy):
		status = 503
	case errors.As(err, &upstream) && (upstream.Status == 503 || (upstream.Status >= 400 && upstream.Status < 500)):
		status = upstream.Status
	}
	writeError(w, status, publicExecutionError(err))
}
func requestError(w http.ResponseWriter, err error) {
	var size *http.MaxBytesError
	if errors.As(err, &size) {
		writeError(w, 413, "request body too large")
		return
	}
	writeError(w, 400, "invalid request: provide messages, model tier, and optional stream boolean")
}
func writeError(w http.ResponseWriter, status int, message string) {
	if (status == 503 || status == 429) && w.Header().Get("Retry-After") == "" {
		w.Header().Set("Retry-After", "1")
	}
	writeJSON(w, status, map[string]any{"error": map[string]string{"message": message, "type": strings.ReplaceAll(http.StatusText(status), " ", "_")}})
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	// JSON/任务接口同样不能被慢速下游无限阻塞。
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(15 * time.Second))
	defer rc.SetWriteDeadline(time.Time{})
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
