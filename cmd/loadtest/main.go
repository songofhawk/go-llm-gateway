// Command loadtest 对本机 LLM gateway 发起有界的闭环压测。
// 它只使用标准库；默认目标是 localhost，避免误压远程或真实付费服务。
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const maxSamples = 100000
const maxJobs = 100000

type options struct {
	url, mode, output, tokenEnv string
	duration, timeout, drain    time.Duration
	concurrency                 int
}

type counters struct {
	mu                                                                      sync.Mutex
	started, success, rejected, canceled, transport, protocol               int64
	statuses                                                                map[string]int64
	pollStatuses                                                            map[string]int64
	latency, acceptanceLatency, rejectedLatency, canceledLatency, firstByte []float64
	jobLatency                                                              []float64
	errorSamples                                                            []string
	sampleDropped                                                           int64
	accepted, jobCompleted, jobFailed                                       int64
}

type report struct {
	ErrorSamples         []string         `json:"error_samples,omitempty"`
	Mode                 string           `json:"mode"`
	Concurrency          int              `json:"concurrency"`
	DurationSeconds      float64          `json:"duration_seconds"`
	LoadSeconds          float64          `json:"load_seconds"`
	TotalSeconds         float64          `json:"total_seconds"`
	LatencyUnit          string           `json:"latency_unit"`
	RequestCounts        map[string]int64 `json:"request_counts"`
	HTTPStatusCounts     map[string]int64 `json:"http_status_counts"`
	PollHTTPStatusCounts map[string]int64 `json:"poll_http_status_counts,omitempty"`
	LatencyMS            map[string]any   `json:"latency_ms"`
	Jobs                 map[string]any   `json:"jobs,omitempty"`
	SampleLimit          int              `json:"sample_limit"`
	SampleDropped        int64            `json:"sample_dropped"`
	MemoryBoundNote      string           `json:"memory_bound_note"`
	SubmittedRPS         float64          `json:"submitted_rps"`
	SuccessRPS           float64          `json:"success_rps"`
	AcceptedRPS          float64          `json:"accepted_rps,omitempty"`
}

type jobRef struct {
	ID      string
	Started time.Time
}
type jobState struct {
	ID    string `json:"id"`
	State string `json:"state"`
}
type pollJobState struct {
	State     string    `json:"state"`
	UpdatedAt time.Time `json:"updated_at"`
}
type firstByteReader struct {
	r       io.Reader
	started time.Time
	first   *time.Time
}

func (r firstByteReader) Read(p []byte) (int, error) {
	n, e := r.r.Read(p)
	if n > 0 && r.first.IsZero() {
		*r.first = time.Now()
	}
	return n, e
}

func main() {
	var o options
	flag.StringVar(&o.url, "url", "http://localhost:8080", "本机 gateway 地址（仅允许 localhost 或数值回环地址）")
	flag.StringVar(&o.mode, "mode", "unary", "压测模式：unary、stream、cancel 或 jobs")
	flag.DurationVar(&o.duration, "duration", 10*time.Second, "调度新请求的时间窗口")
	flag.IntVar(&o.concurrency, "concurrency", 32, "固定闭环 worker 数")
	flag.DurationVar(&o.timeout, "timeout", 30*time.Second, "单次 HTTP 调用超时")
	flag.DurationVar(&o.drain, "drain", 60*time.Second, "jobs 模式提交结束后的后台任务排空预算")
	flag.StringVar(&o.output, "output", "", "可选 JSON 报告文件路径")
	flag.StringVar(&o.tokenEnv, "token-env", "GATEWAY_TOKEN", "读取 Bearer token 的环境变量名；空值表示不发 token")
	flag.Parse()
	if err := run(o); err != nil {
		fmt.Fprintln(os.Stderr, "loadtest:", err)
		os.Exit(2)
	}
}

func validateOptions(o options) (*url.URL, error) {
	if o.mode != "unary" && o.mode != "stream" && o.mode != "cancel" && o.mode != "jobs" {
		return nil, fmt.Errorf("unknown mode %q", o.mode)
	}
	if o.duration <= 0 || o.timeout <= 0 || o.drain <= 0 || o.concurrency <= 0 {
		return nil, errors.New("duration, timeout, drain and concurrency must be positive")
	}
	u, err := url.Parse(o.url)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("url must be an http(s) origin with no credentials, query, or fragment")
	}
	if !isLoopbackHost(u.Hostname()) {
		return nil, errors.New("url host must be localhost or a numeric loopback address")
	}
	return u, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func run(o options) error {
	u, err := validateOptions(o)
	if err != nil {
		return err
	}
	base := strings.TrimRight(u.String(), "/")
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = o.concurrency + 8
	transport.MaxIdleConns = 2 * (o.concurrency + 8)
	transport.DialContext = (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	client := &http.Client{Transport: transport, Timeout: 0, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	stats := &counters{statuses: map[string]int64{}, pollStatuses: map[string]int64{}}
	start := time.Now()
	loadEnd := start.Add(o.duration)
	loadCtx, stopLoad := context.WithDeadline(context.Background(), loadEnd)
	var started atomic.Int64
	var jobMu sync.Mutex
	jobs := make([]jobRef, 0)
	var workers sync.WaitGroup
	for i := 0; i < o.concurrency; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-loadCtx.Done():
					return
				default:
				}
				if o.mode == "jobs" && !reserveJob(&started) {
					return
				}
				if o.mode != "jobs" {
					started.Add(1)
				}
				if !doRequest(client, base, o, stats, &jobMu, &jobs) {
					return
				}
			}
		}()
	}
	<-loadCtx.Done()
	stopLoad()
	workers.Wait()
	loadSeconds := time.Since(start).Seconds()
	if loadSeconds > o.duration.Seconds() {
		loadSeconds = o.duration.Seconds()
	}
	if o.mode == "jobs" {
		drainJobs(client, base, o, stats, jobs)
	}
	totalSeconds := time.Since(start).Seconds()
	out := stats.makeReport(o, loadSeconds, totalSeconds)
	if o.output != "" {
		f, e := os.Create(o.output)
		if e != nil {
			return fmt.Errorf("create output: %w", e)
		}
		encErr := json.NewEncoder(f).Encode(out)
		closeErr := f.Close()
		if encErr != nil {
			return fmt.Errorf("write output: %w", encErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close output: %w", closeErr)
		}
		return nil
	}
	return json.NewEncoder(os.Stdout).Encode(out)
}

func reserveJob(n *atomic.Int64) bool {
	for {
		old := n.Load()
		if old >= maxJobs {
			return false
		}
		if n.CompareAndSwap(old, old+1) {
			return true
		}
	}
}

func doRequest(client *http.Client, base string, o options, s *counters, jobMu *sync.Mutex, jobs *[]jobRef) bool {
	started := time.Now()
	path, stream := "/v1/chat/completions", o.mode == "stream" || o.mode == "cancel"
	if o.mode == "jobs" {
		path = "/v1/jobs"
	}
	body, _ := json.Marshal(map[string]any{"model": "cheap", "messages": []any{map[string]string{"role": "user", "content": "hi"}}, "stream": stream})
	ctx, cancel := context.WithTimeout(context.Background(), o.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		s.addError("transport", time.Since(started), 0)
		return true
	}
	req.Header.Set("Content-Type", "application/json")
	if o.tokenEnv != "" {
		if token := os.Getenv(o.tokenEnv); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
	s.addStarted()
	resp, err := client.Do(req)
	if err != nil {
		s.mu.Lock()
		if len(s.errorSamples) < 8 {
			s.errorSamples = append(s.errorSamples, err.Error())
		}
		s.mu.Unlock()
		s.addError("transport", time.Since(started), 0)
		if ctx.Err() == nil {
			time.Sleep(20 * time.Millisecond)
		}
		return true
	}
	defer resp.Body.Close()
	s.addStatus(resp.StatusCode)
	if resp.StatusCode == 429 || resp.StatusCode == 503 {
		// 读完小错误体才能复用连接；否则压测工具本身会制造额外握手负载。
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		s.addRejected(time.Since(started))
		time.Sleep(20 * time.Millisecond)
		return true
	}
	want := http.StatusOK
	if o.mode == "jobs" {
		want = http.StatusAccepted
	}
	if resp.StatusCode != want {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		s.addError("protocol", time.Since(started), resp.StatusCode)
		time.Sleep(20 * time.Millisecond) // 任何 HTTP 错误都退让，防止 502 导致重试风暴。
		return true
	}
	if o.mode == "cancel" {
		var b [1]byte
		n, e := resp.Body.Read(b[:])
		if n > 0 {
			cancel()
			s.addCanceled(time.Since(started))
			time.Sleep(20 * time.Millisecond) // 取消会关闭连接；避免把重连风暴当成推理吞吐。
			return true
		}
		if e != nil {
			s.addError("transport", time.Since(started), resp.StatusCode)
			return true
		}
		s.addError("protocol", time.Since(started), resp.StatusCode)
		return true
	}
	if o.mode == "stream" {
		first := time.Time{}
		var firstByte [1]byte
		n, readErr := resp.Body.Read(firstByte[:])
		if n == 0 {
			if readErr != nil {
				s.addReadError(time.Since(started), 0, resp.StatusCode)
			} else {
				s.addProtocolLatency(time.Since(started), 0, resp.StatusCode)
			}
			return true
		}
		first = time.Now()
		reader := io.MultiReader(bytes.NewReader(firstByte[:n]), resp.Body)
		scanner := bufio.NewScanner(firstByteReader{r: reader, started: started, first: &first})
		scanner.Buffer(make([]byte, 4096), 1<<20)
		done := false
		afterDone := false
		eventData := make([]string, 0, 2)
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				if len(eventData) > 0 {
					value := strings.Join(eventData, "\n")
					if afterDone {
						done = false
					}
					if value == "[DONE]" {
						done = true
						afterDone = true
					} else if afterDone {
						done = false
					}
					eventData = eventData[:0]
				}
				continue
			}
			if strings.HasPrefix(line, "data:") {
				value := strings.TrimPrefix(line, "data:")
				if strings.HasPrefix(value, " ") {
					value = value[1:]
				}
				eventData = append(eventData, value)
			}
		}
		elapsed := time.Since(started)
		var firstMs float64
		if !first.IsZero() {
			firstMs = first.Sub(started).Seconds() * 1000
		}
		if scanner.Err() != nil {
			s.addReadError(elapsed, firstMs, resp.StatusCode)
			return true
		}
		// [DONE] 必须由空行结束；EOF 时未结束的 data 行不能算完成事件。
		if len(eventData) > 0 {
			done = false
			eventData = eventData[:0]
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			s.addReadError(elapsed, firstMs, resp.StatusCode)
			return true
		}
		if !done {
			s.addProtocolLatency(elapsed, firstMs, resp.StatusCode)
			return true
		}
		s.addSuccess(elapsed, firstMs, resp.StatusCode)
		return true
	}
	if o.mode == "jobs" {
		data, e := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
		elapsed := time.Since(started)
		if e != nil {
			s.addReadError(elapsed, 0, resp.StatusCode)
			return true
		}
		if len(data) > 1<<20 {
			s.addProtocolLatency(elapsed, 0, resp.StatusCode)
			return true
		}
		var j jobState
		if json.Unmarshal(data, &j) != nil || j.ID == "" {
			s.addProtocolLatency(elapsed, 0, resp.StatusCode)
			return true
		}
		s.addAccepted(elapsed)
		jobMu.Lock()
		*jobs = append(*jobs, jobRef{ID: j.ID, Started: started})
		jobMu.Unlock()
		return true
	}
	data, e := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	elapsed := time.Since(started)
	if e != nil {
		s.addReadError(elapsed, 0, resp.StatusCode)
		return true
	}
	if len(data) == 0 || len(data) > 1<<20 || !json.Valid(data) {
		s.addProtocolLatency(elapsed, 0, resp.StatusCode)
		return true
	}
	s.addSuccess(elapsed, 0, resp.StatusCode)
	return true
}

func drainJobs(client *http.Client, base string, o options, s *counters, jobs []jobRef) {
	if len(jobs) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), o.drain)
	defer cancel()
	queue := make(chan jobRef)
	var wg sync.WaitGroup
	workers := o.concurrency
	if workers > 8 {
		workers = 8
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case j, ok := <-queue:
					if !ok {
						return
					}
					pollJob(ctx, client, base, o, s, j)
				}
			}
		}()
	}
send:
	for _, j := range jobs {
		select {
		case queue <- j:
		case <-ctx.Done():
			break send
		}
	}
	close(queue)
	wg.Wait()
}

func pollJob(ctx context.Context, client *http.Client, base string, o options, s *counters, j jobRef) {
	for {
		if ctx.Err() != nil {
			return
		}
		callCtx, cancel := context.WithTimeout(ctx, o.timeout)
		req, err := http.NewRequestWithContext(callCtx, http.MethodGet, base+"/v1/jobs/"+url.PathEscape(j.ID), nil)
		if err == nil && o.tokenEnv != "" {
			if token := os.Getenv(o.tokenEnv); token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
		}
		var resp *http.Response
		if err == nil {
			resp, err = client.Do(req)
		}
		if err != nil {
			cancel()
			if ctx.Err() != nil {
				return
			}
			s.addJobPollError("transport")
			sleepContext(ctx, 100*time.Millisecond)
			continue
		}
		s.addPollStatus(resp.StatusCode)
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
		resp.Body.Close()
		cancel()
		if readErr != nil {
			s.addJobPollError("transport")
			sleepContext(ctx, 100*time.Millisecond)
			continue
		}
		if resp.StatusCode == 429 || resp.StatusCode == 503 {
			s.addJobPollError("rejected")
			sleepContext(ctx, 20*time.Millisecond)
			continue
		}
		if resp.StatusCode != http.StatusOK || len(data) > 1<<20 {
			s.addJobPollError("protocol")
			sleepContext(ctx, 100*time.Millisecond)
			continue
		}
		var state pollJobState
		if json.Unmarshal(data, &state) != nil {
			s.addJobPollError("protocol")
			sleepContext(ctx, 100*time.Millisecond)
			continue
		}
		switch state.State {
		case "done":
			end := state.UpdatedAt
			if end.IsZero() {
				end = time.Now()
			}
			s.addJobTerminal(true, end.Sub(j.Started))
			return
		case "failed":
			end := state.UpdatedAt
			if end.IsZero() {
				end = time.Now()
			}
			s.addJobTerminal(false, end.Sub(j.Started))
			return
		case "queued", "running", "processing":
			sleepContext(ctx, 100*time.Millisecond)
		case "completed":
			sleepContext(ctx, 100*time.Millisecond)
		default:
			s.addJobPollError("protocol")
			sleepContext(ctx, 100*time.Millisecond)
		}
	}
}

func sleepContext(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func (s *counters) addStarted()        { s.mu.Lock(); s.started++; s.mu.Unlock() }
func (s *counters) addStatus(code int) { s.mu.Lock(); s.statuses[fmt.Sprint(code)]++; s.mu.Unlock() }
func (s *counters) addPollStatus(code int) {
	s.mu.Lock()
	s.pollStatuses[fmt.Sprint(code)]++
	s.mu.Unlock()
}
func (s *counters) sample(dst *[]float64, v float64) {
	if len(*dst) < maxSamples {
		*dst = append(*dst, v)
	} else {
		s.sampleDropped++
	}
}
func (s *counters) addSuccess(d time.Duration, first float64, code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.success++
	s.sample(&s.latency, d.Seconds()*1000)
	if first > 0 {
		s.sample(&s.firstByte, first)
	}
	_ = code
}
func (s *counters) addError(kind string, d time.Duration, code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if kind == "transport" {
		s.transport++
	} else {
		s.protocol++
	}
	_ = d
	_ = code
}
func (s *counters) addReadError(d time.Duration, first float64, code int) {
	s.addError("transport", d, code)
	if first > 0 {
		s.mu.Lock()
		s.sample(&s.firstByte, first)
		s.mu.Unlock()
	}
}
func (s *counters) addProtocolLatency(d time.Duration, first float64, code int) {
	s.addError("protocol", d, code)
	if first > 0 {
		s.mu.Lock()
		s.sample(&s.firstByte, first)
		s.mu.Unlock()
	}
}
func (s *counters) addRejected(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rejected++
	s.sample(&s.rejectedLatency, d.Seconds()*1000)
}
func (s *counters) addCanceled(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.canceled++
	s.sample(&s.canceledLatency, d.Seconds()*1000)
}
func (s *counters) addAccepted(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accepted++
	s.sample(&s.acceptanceLatency, d.Seconds()*1000)
}
func (s *counters) addJobTerminal(ok bool, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ok {
		s.jobCompleted++
	} else {
		s.jobFailed++
	}
	s.sample(&s.jobLatency, d.Seconds()*1000)
}
func (s *counters) addJobPollError(kind string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch kind {
	case "transport":
		s.transport++
	case "protocol":
		s.protocol++
	case "rejected":
		s.rejected++
	}
}

func percentiles(values []float64) map[string]float64 {
	if len(values) == 0 {
		return map[string]float64{"p50": 0, "p95": 0, "p99": 0}
	}
	v := append([]float64(nil), values...)
	sort.Float64s(v)
	get := func(p float64) float64 { i := int(float64(len(v)-1) * p); return v[i] }
	return map[string]float64{"p50": get(.50), "p95": get(.95), "p99": get(.99)}
}

func (s *counters) makeReport(o options, load, total float64) report {
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := map[string]int64{"started": s.started, "success": s.success, "rejected": s.rejected, "expected_canceled": s.canceled, "transport_errors": s.transport, "protocol_errors": s.protocol, "accepted": s.accepted}
	pending := int64(0)
	if o.mode == "jobs" {
		pending = s.accepted - s.jobCompleted - s.jobFailed
		if pending < 0 {
			pending = 0
		}
	}
	jobs := map[string]any(nil)
	if o.mode == "jobs" {
		jobs = map[string]any{"accepted": s.accepted, "completed": s.jobCompleted, "failed": s.jobFailed, "pending": pending, "end_to_end_completion_ms": percentiles(s.jobLatency)}
	}
	var successRPS float64
	if total > 0 {
		successRPS = float64(s.success) / total
		if o.mode == "jobs" {
			successRPS = float64(s.jobCompleted) / total
		}
	}
	var submittedRPS float64
	if load > 0 {
		submittedRPS = float64(s.started) / load
	}
	out := report{ErrorSamples: append([]string(nil), s.errorSamples...), Mode: o.mode, Concurrency: o.concurrency, DurationSeconds: o.duration.Seconds(), LoadSeconds: load, TotalSeconds: total, LatencyUnit: "milliseconds", RequestCounts: counts, HTTPStatusCounts: cloneMap(s.statuses), PollHTTPStatusCounts: cloneMap(s.pollStatuses), LatencyMS: map[string]any{"first_byte": percentiles(s.firstByte), "success_complete": percentiles(s.latency), "rejected": percentiles(s.rejectedLatency), "expected_canceled": percentiles(s.canceledLatency), "job_acceptance": percentiles(s.acceptanceLatency)}, Jobs: jobs, SampleLimit: maxSamples, SampleDropped: s.sampleDropped, MemoryBoundNote: "每类延迟最多保留 100000 个样本；jobs 最多提交并保存 100000 个任务 ID。任务完成时间依据服务端 updated_at，要求客户端与网关时钟同步。", SubmittedRPS: submittedRPS, SuccessRPS: successRPS}
	if o.mode == "jobs" && load > 0 {
		out.AcceptedRPS = float64(s.accepted) / load
	}
	return out
}
func cloneMap(in map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
