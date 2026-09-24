package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"example.com/llm-gateway/internal/jobs"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type providerFunc func(context.Context, string, Request) (io.ReadCloser, error)

func (f providerFunc) Open(c context.Context, m string, r Request) (io.ReadCloser, error) {
	return f(c, m, r)
}
func request(t testing.TB, stream bool) Request {
	t.Helper()
	r, e := ParseRequest([]byte(fmt.Sprintf(`{"model":"cheap","messages":[{}],"stream":%t}`, stream)))
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func testGateway(t testing.TB, ps map[string]Provider, groups [][]Target, capacity int, times Timeouts) *Gateway {
	t.Helper()
	cfg := Config{Routes: map[string][][]Target{}}
	for name := range ps {
		cfg.Endpoints = append(cfg.Endpoints, Endpoint{Name: name, Capacity: capacity})
	}
	for _, tier := range []string{"cheap", "balanced", "powerful"} {
		cfg.Routes[tier] = groups
	}
	g, e := New(cfg, ps, times)
	if e != nil {
		t.Fatal(e)
	}
	return g
}
func oneGroup(names ...string) [][]Target {
	group := []Target{}
	for _, n := range names {
		group = append(group, Target{Provider: n, Model: "real-" + n})
	}
	return [][]Target{group}
}
func fallbackGroups() [][]Target {
	return [][]Target{{{Provider: "p", Model: "p"}}, {{Provider: "b", Model: "b"}}}
}
func constantProvider() Provider {
	return providerFunc(func(context.Context, string, Request) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(`{"choices":[]}`)), nil
	})
}
func assertFree(t testing.TB, g *Gateway) {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	for name, b := range g.backends {
		if b.active != 0 {
			t.Errorf("%s leaked %d slots", name, b.active)
		}
	}
}
func clientProvider(t testing.TB, h http.HandlerFunc, times Timeouts) *OpenAIProvider {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 模拟真实模型服务先读完请求，HTTP/1 才能继续监测对端断开。
		// 单纯等待 Context 而不消费 body，会让测试服务本身阻塞取消检测。
		data, _ := io.ReadAll(r.Body)
		r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(data))
		h(w, r)
	}))
	t.Cleanup(s.Close)
	c := NewHTTPClient(times.Header)
	t.Cleanup(c.CloseIdleConnections)
	return &OpenAIProvider{URL: s.URL + "/v1", Key: "provider-secret", Client: c}
}
func TestParseRequest(t *testing.T) {
	for _, s := range []string{`null`, `[]`, `{}`, `{"messages":[]}`, `{"model":"unknown","messages":[{}]}`, `{"messages":[{}],"stream":"yes"}`, `{"messages":[{}],"stream":null}`, `{"model":null,"messages":[{}]}`} {
		if _, e := ParseRequest([]byte(s)); e == nil {
			t.Errorf("accepted %s", s)
		}
	}
	r, e := ParseRequest([]byte(`{"messages":[{}],"tools":[{}]}`))
	if e != nil || r.Tier != "balanced" || r.Fields["tools"] == nil {
		t.Fatalf("%+v %v", r, e)
	}
}
func TestProviderMappingAndCredentialIsolation(t *testing.T) {
	times := DefaultTimeouts()
	p := clientProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer provider-secret" {
			t.Error("wrong path/credentials")
		}
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		if string(body["model"]) != `"real-p"` || body["tools"] == nil {
			t.Error("lost model/tools")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{}`)
	}, times)
	g := testGateway(t, map[string]Provider{"p": p}, oneGroup("p"), 1, times)
	s := httptest.NewServer(NewAPI(g, nil, "client-secret", 1, 1).Handler())
	defer s.Close()
	req, _ := http.NewRequest("POST", s.URL+"/v1/chat/completions", strings.NewReader(`{"model":"cheap","messages":[{}],"tools":[]}`))
	req.Header.Set("Authorization", "Bearer client-secret")
	resp, e := http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	assertFree(t, g)
}
func TestFallbackStatusAndFailureRelease(t *testing.T) {
	for _, status := range []int{400, 401, 403, 408, 429, 500, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			p := providerFunc(func(context.Context, string, Request) (io.ReadCloser, error) {
				return nil, &UpstreamError{Status: status}
			})
			b := providerFunc(func(context.Context, string, Request) (io.ReadCloser, error) {
				calls.Add(1)
				return io.NopCloser(strings.NewReader(`{}`)), nil
			})
			g := testGateway(t, map[string]Provider{"p": p, "b": b}, fallbackGroups(), 1, DefaultTimeouts())
			res, e := g.Do(context.Background(), request(t, false))
			want := status == 408 || status == 429 || status >= 500
			if want {
				if e != nil {
					t.Fatal(e)
				}
				res.Body.Close()
				if calls.Load() != 1 {
					t.Fatal("no fallback")
				}
			} else if e == nil || calls.Load() != 0 {
				t.Fatal("unexpected fallback")
			}
			assertFree(t, g)
		})
	}
}
func TestLoadBalanceCapacityTracksWholeStream(t *testing.T) {
	g := testGateway(t, map[string]Provider{"a": constantProvider(), "b": constantProvider()}, oneGroup("a", "b"), 1, DefaultTimeouts())
	a, e := g.Do(context.Background(), request(t, true))
	if e != nil {
		t.Fatal(e)
	}
	b, e := g.Do(context.Background(), request(t, true))
	if e != nil {
		t.Fatal(e)
	}
	if a.Provider == b.Provider {
		t.Fatal("not balanced")
	}
	if _, e := g.Do(context.Background(), request(t, true)); !errors.Is(e, ErrBusy) {
		t.Fatal(e)
	}
	a.Body.Close()
	a.Body.Close()
	c, e := g.Do(context.Background(), request(t, true))
	if e != nil {
		t.Fatal(e)
	}
	b.Body.Close()
	c.Body.Close()
	assertFree(t, g)
}
func TestCancellationAndTimeoutRelease(t *testing.T) {
	for _, kind := range []string{"cancel", "header", "idle", "attempt"} {
		t.Run(kind, func(t *testing.T) {
			ts := DefaultTimeouts()
			ts.Header = time.Second
			ts.Idle = time.Second
			ts.Attempt = time.Second
			switch kind {
			case "header":
				ts.Header = 30 * time.Millisecond
			case "idle":
				ts.Idle = 30 * time.Millisecond
			case "attempt":
				ts.Attempt = 30 * time.Millisecond
			}
			started, ended := make(chan struct{}), make(chan struct{})
			p := clientProvider(t, func(w http.ResponseWriter, r *http.Request) {
				defer close(ended)
				if kind == "idle" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
				}
				close(started)
				<-r.Context().Done()
			}, ts)
			g := testGateway(t, map[string]Provider{"p": p}, oneGroup("p"), 1, ts)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			req := request(t, false)
			go func() { _, e := g.Do(ctx, req); done <- e }()
			<-started
			if kind == "cancel" {
				cancel()
			}
			select {
			case e := <-done:
				if e == nil {
					t.Fatal("missing error")
				}
				if kind != "cancel" && !errors.Is(e, context.DeadlineExceeded) {
					t.Fatalf("timeout lost its identity: %v", e)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("request leaked")
			}
			select {
			case <-ended:
			case <-time.After(time.Second):
				t.Fatal("upstream leaked")
			}
			assertFree(t, g)
		})
	}
}
func TestAttemptTimeoutCanFallback(t *testing.T) {
	ts := DefaultTimeouts()
	ts.Attempt = 30 * time.Millisecond
	p := providerFunc(func(c context.Context, _ string, _ Request) (io.ReadCloser, error) { <-c.Done(); return nil, c.Err() })
	g := testGateway(t, map[string]Provider{"p": p, "b": constantProvider()}, fallbackGroups(), 1, ts)
	res, e := g.Do(context.Background(), request(t, false))
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	if res.Provider != "b" {
		t.Fatal("wrong target")
	}
	assertFree(t, g)
}
func TestStreamingFlushAndDisconnect(t *testing.T) {
	ts := DefaultTimeouts()
	ended := make(chan struct{})
	p := clientProvider(t, func(w http.ResponseWriter, r *http.Request) {
		defer close(ended)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}, ts)
	g := testGateway(t, map[string]Provider{"p": p}, oneGroup("p"), 1, ts)
	s := httptest.NewServer(NewAPI(g, nil, "", 1, 1).Handler())
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", s.URL+"/v1/chat/completions", strings.NewReader(`{"model":"cheap","messages":[{}],"stream":true}`))
	resp, e := http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	line, e := bufio.NewReader(resp.Body).ReadString('\n')
	if e != nil || line != "data: first\n" {
		t.Fatalf("not flushed %q %v", line, e)
	}
	if _, e := g.Do(context.Background(), request(t, true)); !errors.Is(e, ErrBusy) {
		t.Fatal("early release")
	}
	cancel()
	resp.Body.Close()
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("disconnect not propagated")
	}
	until := time.Now().Add(time.Second)
	for time.Now().Before(until) {
		g.mu.Lock()
		n := g.backends["p"].active
		g.mu.Unlock()
		if n == 0 {
			assertFree(t, g)
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("slot leaked")
}
func TestTruncatedStreamNeverFallsBack(t *testing.T) {
	ts := DefaultTimeouts()
	var calls atomic.Int32
	p := clientProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: partial\n\n")
	}, ts)
	b := providerFunc(func(context.Context, string, Request) (io.ReadCloser, error) {
		calls.Add(1)
		return nil, errors.New("unexpected")
	})
	g := testGateway(t, map[string]Provider{"p": p, "b": b}, fallbackGroups(), 1, ts)
	s := httptest.NewServer(NewAPI(g, nil, "", 1, 1).Handler())
	defer s.Close()
	resp, e := http.Post(s.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"cheap","messages":[{}],"stream":true}`))
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	data, e := io.ReadAll(resp.Body)
	if e == nil || !strings.Contains(string(data), "partial") || calls.Load() != 0 {
		t.Fatalf("%s %v fallback=%d", data, e, calls.Load())
	}
	assertFree(t, g)
}
func TestDoneDetectorFragmentation(t *testing.T) {
	d := doneDetector{}
	for _, s := range []string{"data: {\"text\":\"[DONE]\"}\n\n", "data: [DO", "NE]\r", "\n", "\r\n"} {
		d.feed([]byte(s))
	}
	if !d.done {
		t.Fatal("missing DONE")
	}
	d = doneDetector{}
	d.feed([]byte("data: " + strings.Repeat("x", 1<<20) + "[DONE]\n\n"))
	if d.done || cap(d.line) > 64 {
		t.Fatal("unbounded/false positive")
	}
}
func TestHTTPAdmissionValidation(t *testing.T) {
	g := testGateway(t, map[string]Provider{"p": constantProvider()}, oneGroup("p"), 1, DefaultTimeouts())
	a := NewAPI(g, nil, "token", 1, 1)
	for _, tc := range []struct {
		body, auth string
		status     int
	}{{`{"messages":[{}]}`, "", 401}, {`{}`, "Bearer token", 400}, {strings.Repeat("x", MaxRequestBytes+1), "Bearer token", 413}} {
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(tc.body))
		r.Header.Set("Authorization", tc.auth)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Errorf("got %d want %d", w.Code, tc.status)
		}
	}
	a.unary <- struct{}{}
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"messages":[{}]}`))
	r.Header.Set("Authorization", "Bearer token")
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
}
func TestConcurrentCallsBoundedRaceFree(t *testing.T) {
	var active, peak atomic.Int32
	p := providerFunc(func(context.Context, string, Request) (io.ReadCloser, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for {
			o := peak.Load()
			if o >= n || peak.CompareAndSwap(o, n) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		return io.NopCloser(strings.NewReader(`{}`)), nil
	})
	g := testGateway(t, map[string]Provider{"p": p}, oneGroup("p"), 8, DefaultTimeouts())
	req := request(t, false)
	var wg sync.WaitGroup
	for i := 0; i < 256; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, e := g.Do(context.Background(), req)
			if e == nil {
				res.Body.Close()
			} else if !errors.Is(e, ErrBusy) {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	if peak.Load() > 8 || peak.Load() < 2 {
		t.Fatal(peak.Load())
	}
	assertFree(t, g)
}
func TestAsyncHTTPIntegration(t *testing.T) {
	store, e := jobs.Open(t.TempDir()+"/jobs.db", 2)
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	g := testGateway(t, map[string]Provider{"p": constantProvider()}, oneGroup("p"), 2, DefaultTimeouts())
	s := httptest.NewServer(NewAPI(g, store, "", 2, 2).Handler())
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- store.Run(ctx, 1, g.ExecuteJob) }()
	defer func() { cancel(); <-done }()
	resp, e := http.Post(s.URL+"/v1/jobs", "application/json", strings.NewReader(`{"model":"cheap","messages":[{}]}`))
	if e != nil {
		t.Fatal(e)
	}
	var job jobs.Job
	e = json.NewDecoder(resp.Body).Decode(&job)
	resp.Body.Close()
	if e != nil || resp.StatusCode != 202 {
		t.Fatalf("%d %v", resp.StatusCode, e)
	}
	until := time.Now().Add(3 * time.Second)
	for time.Now().Before(until) {
		resp, e := http.Get(s.URL + "/v1/jobs/" + job.ID)
		if e != nil {
			t.Fatal(e)
		}
		e = json.NewDecoder(resp.Body).Decode(&job)
		resp.Body.Close()
		if e != nil {
			t.Fatal(e)
		}
		if job.State == "done" {
			if !job.Processed || !json.Valid(job.Result) {
				t.Fatal("missing saved result")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("unfinished %+v", job)
}
func TestInvalidResponseClosesAndFallsBack(t *testing.T) {
	var closed atomic.Bool
	p := providerFunc(func(context.Context, string, Request) (io.ReadCloser, error) {
		return &trackedBody{Reader: strings.NewReader("bad"), closed: &closed}, nil
	})
	g := testGateway(t, map[string]Provider{"p": p, "b": constantProvider()}, fallbackGroups(), 1, DefaultTimeouts())
	res, e := g.Do(context.Background(), request(t, false))
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	if !closed.Load() {
		t.Fatal("body leaked")
	}
	assertFree(t, g)
}

type trackedBody struct {
	io.Reader
	closed *atomic.Bool
}

func (b *trackedBody) Close() error { b.closed.Store(true); return nil }
func TestResponseLimit(t *testing.T) {
	p := providerFunc(func(context.Context, string, Request) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("x"), MaxResponseBytes+1))), nil
	})
	g := testGateway(t, map[string]Provider{"p": p}, oneGroup("p"), 1, DefaultTimeouts())
	if _, e := g.Do(context.Background(), request(t, false)); !errors.Is(e, ErrTooLarge) {
		t.Fatal(e)
	}
	assertFree(t, g)
}
func TestProviderRejectsRedirectAndWrongContentType(t *testing.T) {
	for _, kind := range []string{"redirect", "content"} {
		t.Run(kind, func(t *testing.T) {
			p := clientProvider(t, func(w http.ResponseWriter, r *http.Request) {
				if kind == "redirect" {
					w.Header().Set("Location", "https://example.invalid")
					w.WriteHeader(302)
					return
				}
				w.Header().Set("Content-Type", "text/html")
				fmt.Fprint(w, "oops")
			}, DefaultTimeouts())
			if b, e := p.Open(context.Background(), "m", request(t, false)); e == nil {
				b.Close()
				t.Fatal("accepted")
			}
		})
	}
}
func BenchmarkGatewayParallel(b *testing.B) {
	g := testGateway(b, map[string]Provider{"p": constantProvider()}, oneGroup("p"), 1024, DefaultTimeouts())
	r := request(b, false)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			res, e := g.Do(context.Background(), r)
			if e != nil {
				b.Error(e)
				continue
			}
			_, _ = io.Copy(io.Discard, res.Body)
			res.Body.Close()
		}
	})
}

func TestDoneDetectorWholeEventAndLineEndings(t *testing.T) {
	for _, tc := range []struct {
		input  string
		done   bool
		length int
	}{
		{"data: partial\ndata: [DONE]\n\n", false, 0},
		{"data: [DONE]\ndata: more\n\n", false, 0},
		{"data: [DONE]\r\r", true, len("data: [DONE]\r\r")},
		{"data: [DONE]\n\ndata: after\n\n", true, len("data: [DONE]\n\n")},
		{"data: [DONE]\r\n\r\ndata: after\n\n", true, len("data: [DONE]\r\n\r\n")},
	} {
		d := doneDetector{}
		n := d.feed([]byte(tc.input))
		if d.done != tc.done || (tc.done && n != tc.length) {
			t.Errorf("%q => %v %d", tc.input, d.done, n)
		}
	}
}
func TestCompleteStreamStopsAtDone(t *testing.T) {
	ts := DefaultTimeouts()
	p := clientProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: token\n\ndata: [DONE]\n\ndata: after\n\n")
	}, ts)
	g := testGateway(t, map[string]Provider{"p": p}, oneGroup("p"), 1, ts)
	s := httptest.NewServer(NewAPI(g, nil, "", 1, 1).Handler())
	defer s.Close()
	resp, e := http.Post(s.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"cheap","messages":[{}],"stream":true}`))
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	data, e := io.ReadAll(resp.Body)
	if e != nil || string(data) != "data: token\n\ndata: [DONE]\n\n" {
		t.Fatalf("%q %v", data, e)
	}
	assertFree(t, g)
}
func TestEmptyStreamCanFallback(t *testing.T) {
	p := providerFunc(func(context.Context, string, Request) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("")), nil
	})
	g := testGateway(t, map[string]Provider{"p": p, "b": constantProvider()}, fallbackGroups(), 1, DefaultTimeouts())
	res, e := g.Do(context.Background(), request(t, true))
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	if res.Provider != "b" {
		t.Fatal("missing fallback")
	}
	assertFree(t, g)
}
func TestSlowDownstreamReleasesResources(t *testing.T) {
	ts := DefaultTimeouts()
	ts.Write = 30 * time.Millisecond
	ts.Idle = 3 * time.Second
	ts.Attempt = 5 * time.Second
	ended := make(chan struct{})
	p := clientProvider(t, func(w http.ResponseWriter, r *http.Request) {
		defer close(ended)
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := "data: " + strings.Repeat("x", 32<<10) + "\n\n"
		rc := http.NewResponseController(w)
		for {
			if r.Context().Err() != nil {
				return
			}
			_ = rc.SetWriteDeadline(time.Now().Add(time.Second))
			if _, e := io.WriteString(w, chunk); e != nil {
				return
			}
			if e := rc.Flush(); e != nil {
				return
			}
		}
	}, ts)
	g := testGateway(t, map[string]Provider{"p": p}, oneGroup("p"), 1, ts)
	s := httptest.NewServer(NewAPI(g, nil, "", 1, 1).Handler())
	defer s.Close()
	resp, e := http.Post(s.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"cheap","messages":[{}],"stream":true}`))
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	// 客户端故意不读取 body，直到 TCP 背压使网关写入达到 deadline。
	select {
	case <-ended:
	case <-time.After(3 * time.Second):
		t.Fatal("slow client kept upstream alive")
	}
	until := time.Now().Add(time.Second)
	for time.Now().Before(until) {
		g.mu.Lock()
		n := g.backends["p"].active
		g.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("slow client slot leak")
}

func TestTotalBudgetStopsFallback(t *testing.T) {
	ts := DefaultTimeouts()
	ts.Total = 20 * time.Millisecond
	ts.Attempt = time.Second
	var backup atomic.Int32
	p := providerFunc(func(ctx context.Context, _ string, _ Request) (io.ReadCloser, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	b := providerFunc(func(context.Context, string, Request) (io.ReadCloser, error) {
		backup.Add(1)
		return io.NopCloser(strings.NewReader(`{}`)), nil
	})
	g := testGateway(t, map[string]Provider{"p": p, "b": b}, fallbackGroups(), 1, ts)
	if _, err := g.Do(context.Background(), request(t, false)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if backup.Load() != 0 {
		t.Fatal("retried past request budget")
	}
	assertFree(t, g)
}

func TestShutdownCancelsActiveStreams(t *testing.T) {
	ts := DefaultTimeouts()
	ended := make(chan struct{})
	p := clientProvider(t, func(w http.ResponseWriter, r *http.Request) {
		defer close(ended)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: token\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}, ts)
	g := testGateway(t, map[string]Provider{"p": p}, oneGroup("p"), 1, ts)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := httptest.NewUnstartedServer(NewAPI(g, nil, "", 1, 1).Handler())
	s.Config.BaseContext = func(net.Listener) context.Context { return ctx }
	s.Start()
	defer s.Close()
	resp, err := http.Post(s.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"cheap","messages":[{}],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	shutdown, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stop()
	// 与入口相同：Shutdown 等待宽限期，随后取消 BaseContext 并关闭连接。
	if err := s.Config.Shutdown(shutdown); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected active stream during drain: %v", err)
	}
	cancel()
	_ = s.Config.Close()
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("shutdown leaked upstream")
	}
	until := time.Now().Add(time.Second)
	for time.Now().Before(until) {
		g.mu.Lock()
		n := g.backends["p"].active
		g.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("shutdown slot leak")
}

func TestSmallErrorBodyReusesConnection(t *testing.T) {
	var connections atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
		http.Error(w, "busy", 503)
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	defer server.Close()
	client := NewHTTPClient(time.Second)
	defer client.CloseIdleConnections()
	p := &OpenAIProvider{URL: server.URL + "/v1", Client: client}
	for i := 0; i < 10; i++ {
		if body, err := p.Open(context.Background(), "m", request(t, false)); err == nil {
			body.Close()
			t.Fatal("expected 503")
		}
	}
	if connections.Load() != 1 {
		t.Fatalf("small error responses opened %d connections", connections.Load())
	}
}
func TestErrorBodyDrainIsTimeBounded(t *testing.T) {
	p := clientProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}, DefaultTimeouts())
	start := time.Now()
	if body, err := p.Open(context.Background(), "m", request(t, false)); err == nil {
		body.Close()
		t.Fatal("expected error")
	}
	if time.Since(start) > time.Second {
		t.Fatal("stalled error body blocked fallback")
	}
}

func TestUpstreamUnavailableKeepsRetryableStatus(t *testing.T) {
	w := httptest.NewRecorder()
	executionError(w, &UpstreamError{Status: 503})
	if w.Code != 503 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("unavailable upstream became %d", w.Code)
	}
}
