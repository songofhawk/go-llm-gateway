package gateway

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"example.com/llm-gateway/internal/jobs"
)

func TestRateLimiterConfig(t *testing.T) {
	l, err := NewRateLimiter(0, 0)
	if err != nil || l != nil {
		t.Fatal("disabled config rejected")
	}
	if ok, _ := l.Allow(); !ok {
		t.Fatal("nil limiter should allow")
	}
	for _, tc := range []struct {
		rate  float64
		burst int
	}{{-1, 1}, {1, 0}, {0, 1}, {1, -1}, {math.NaN(), 1}, {math.Inf(1), 1}, {1e-8, 1}, {1e7, 1}, {1, 1000001}} {
		if _, err := NewRateLimiter(tc.rate, tc.burst); err == nil {
			t.Errorf("accepted invalid config %+v", tc)
		}
	}
}
func TestRateLimiterRefillAndBurst(t *testing.T) {
	l, _ := NewRateLimiter(2, 2)
	now := l.last
	for i := 0; i < 2; i++ {
		if ok, _ := l.allowAt(now); !ok {
			t.Fatal("initial burst missing")
		}
	}
	if ok, retry := l.allowAt(now); ok || retry != 1 {
		t.Fatalf("excess burst: %v %d", ok, retry)
	}
	if ok, _ := l.allowAt(now.Add(250 * time.Millisecond)); ok {
		t.Fatal("fractional token admitted")
	}
	if ok, _ := l.allowAt(now.Add(500 * time.Millisecond)); !ok {
		t.Fatal("refill did not recover")
	}
	// 空闲不会积累超过 burst 的额度；拒绝请求不会预订未来令牌。
	later := now.Add(time.Hour)
	for i := 0; i < 2; i++ {
		if ok, _ := l.allowAt(later); !ok {
			t.Fatal("burst not refilled")
		}
	}
	if ok, _ := l.allowAt(later); ok {
		t.Fatal("tokens grew without bound")
	}
	if ok, _ := l.allowAt(now); ok {
		t.Fatal("clock rollback created tokens")
	}
}
func TestRateLimiterRetryAfterFractionalRate(t *testing.T) {
	l, _ := NewRateLimiter(.5, 1)
	now := l.last
	l.allowAt(now)
	if ok, retry := l.allowAt(now); ok || retry != 2 {
		t.Fatalf("want retry 2: %v %d", ok, retry)
	}
	if ok, retry := l.allowAt(now.Add(time.Second)); ok || retry != 1 {
		t.Fatalf("want retry 1: %v %d", ok, retry)
	}
	if ok, _ := l.allowAt(now.Add(2 * time.Second)); !ok {
		t.Fatal("did not recover")
	}
}
func TestRateLimiterConcurrentBound(t *testing.T) {
	l, _ := NewRateLimiter(10, 37)
	now := l.last
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 512; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := l.allowAt(now); ok {
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 37 {
		t.Fatalf("accepted %d, want exactly 37", accepted.Load())
	}
}
func TestRateLimitHTTPAdmission(t *testing.T) {
	var calls atomic.Int32
	p := providerFunc(func(context.Context, string, Request) (io.ReadCloser, error) {
		calls.Add(1)
		return io.NopCloser(strings.NewReader(`{}`)), nil
	})
	g := testGateway(t, map[string]Provider{"p": p}, oneGroup("p"), 4, DefaultTimeouts())
	a := NewAPI(g, nil, "secret", 4, 4)
	a.Limiter, _ = NewRateLimiter(.001, 1)
	server := httptest.NewServer(a.Handler())
	defer server.Close()
	call := func(auth string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("POST", server.URL+"/v1/chat/completions", strings.NewReader(`{"messages":[{}]}`))
		req.Header.Set("Authorization", auth)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		return res
	}
	if res := call("bad"); res.StatusCode != 401 {
		t.Fatal("auth must happen first")
	}
	if res := call("Bearer secret"); res.StatusCode != 200 {
		t.Fatal(res.StatusCode)
	}
	if res := call("Bearer secret"); res.StatusCode != 429 || res.Header.Get("Retry-After") != "1000" {
		t.Fatalf("rate response %d retry %s", res.StatusCode, res.Header.Get("Retry-After"))
	}
	if calls.Load() != 1 {
		t.Fatal("rate-limited call reached provider")
	}
	assertFree(t, g)
}
func TestRateLimitSharedByJobsAndChatButNotQueries(t *testing.T) {
	store, err := jobs.Open(t.TempDir()+"/jobs.db", 4)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	g := testGateway(t, map[string]Provider{"p": constantProvider()}, oneGroup("p"), 4, DefaultTimeouts())
	a := NewAPI(g, store, "", 4, 4)
	a.Limiter, _ = NewRateLimiter(.001, 1)
	server := httptest.NewServer(a.Handler())
	defer server.Close()
	res, err := http.Post(server.URL+"/v1/jobs", "application/json", strings.NewReader(`{"messages":[{}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var job jobs.Job
	err = json.NewDecoder(res.Body).Decode(&job)
	res.Body.Close()
	if err != nil || res.StatusCode != 202 {
		t.Fatal("job was not accepted")
	}
	for _, path := range []string{"/v1/chat/completions", "/v1/jobs"} {
		res, err := http.Post(server.URL+path, "application/json", strings.NewReader(`{"messages":[{}]}`))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 429 {
			t.Fatalf("%s: %d", path, res.StatusCode)
		}
	}
	for _, path := range []string{"/healthz", "/v1/jobs/" + job.ID} {
		res, err := http.Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("query rate-limited: %s", path)
		}
	}
}
func TestRateLimitDoesNotInterruptAdmittedStream(t *testing.T) {
	finish := make(chan struct{}, 1)
	p := clientProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-finish:
			io.WriteString(w, "data: [DONE]\n\n")
		case <-r.Context().Done():
		}
	}, DefaultTimeouts())
	g := testGateway(t, map[string]Provider{"p": p}, oneGroup("p"), 2, DefaultTimeouts())
	a := NewAPI(g, nil, "", 2, 2)
	a.Limiter, _ = NewRateLimiter(.001, 1)
	s := httptest.NewServer(a.Handler())
	defer s.Close()
	defer close(finish)
	// defer 的顺序确保失败断言也会解除模拟上游等待，不让测试清理卡住。
	first, err := http.Post(s.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"cheap","messages":[{}],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Body.Close()
	var prefix [1]byte
	if _, err := first.Body.Read(prefix[:]); err != nil {
		t.Fatal(err)
	}
	second, err := http.Post(s.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"messages":[{}]}`))
	if err != nil {
		t.Fatal(err)
	}
	second.Body.Close()
	if second.StatusCode != 429 {
		t.Fatal(second.StatusCode)
	}
	finish <- struct{}{}
	data, err := io.ReadAll(first.Body)
	if err != nil || !strings.Contains(string(data), "[DONE]") {
		t.Fatalf("existing stream interrupted: %v", err)
	}
}

func TestRateLimitAdmittedErrorsDoNotRefund(t *testing.T) {
	for _, kind := range []string{"invalid", "capacity", "upstream"} {
		t.Run(kind, func(t *testing.T) {
			p := constantProvider()
			if kind == "upstream" {
				p = providerFunc(func(context.Context, string, Request) (io.ReadCloser, error) { return nil, &UpstreamError{Status: 500} })
			}
			g := testGateway(t, map[string]Provider{"p": p}, oneGroup("p"), 1, DefaultTimeouts())
			a := NewAPI(g, nil, "", 1, 1)
			a.Limiter, _ = NewRateLimiter(.001, 1)
			payload := `{"messages":[{}]}`
			want := 502
			if kind == "invalid" {
				payload = `broken`
				want = 400
			}
			if kind == "capacity" {
				a.unary <- struct{}{}
				want = 503
			}
			handler := a.Handler()
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(payload)))
			if w.Code != want {
				t.Fatal(w.Code)
			}
			w = httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"messages":[{}]}`)))
			if w.Code != 429 {
				t.Fatal("admitted failure refunded its token")
			}
		})
	}
}
func TestRateLimitFallbackConsumesOneToken(t *testing.T) {
	p := providerFunc(func(context.Context, string, Request) (io.ReadCloser, error) { return nil, &UpstreamError{Status: 503} })
	g := testGateway(t, map[string]Provider{"p": p, "b": constantProvider()}, fallbackGroups(), 1, DefaultTimeouts())
	a := NewAPI(g, nil, "", 1, 1)
	a.Limiter, _ = NewRateLimiter(.001, 1)
	s := httptest.NewServer(a.Handler())
	defer s.Close()
	for _, want := range []int{200, 429} {
		res, err := http.Post(s.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"cheap","messages":[{}]}`))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != want {
			t.Fatalf("got %d want %d", res.StatusCode, want)
		}
	}
}
