package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestValidateOptionsAllowsOnlyLoopbackHosts(t *testing.T) {
	for _, raw := range []string{"http://localhost:8080", "https://127.0.0.1:9443", "http://[::1]:8080"} {
		if _, err := validateOptions(options{url: raw, mode: "unary", duration: time.Second, timeout: time.Second, drain: time.Second, concurrency: 1}); err != nil {
			t.Errorf("期望允许回环 URL %q：%v", raw, err)
		}
	}
	for _, raw := range []string{"http://example.com", "http://localhost.evil:8080", "http://127.0.0.2.example:8080", "http://user@localhost:8080", "http://localhost:8080/path?x=1"} {
		if _, err := validateOptions(options{url: raw, mode: "unary", duration: time.Second, timeout: time.Second, drain: time.Second, concurrency: 1}); err == nil {
			t.Errorf("不应允许此 URL %q", raw)
		}
	}
}

func TestStreamRequiresCompleteDoneEvent(t *testing.T) {
	cases := []struct {
		name, body                string
		wantSuccess, wantProtocol int64
	}{
		{name: "完整终止事件", body: "data: {\"choices\":[]}\n\ndata: [DONE]\n\n", wantSuccess: 1},
		{name: "流被截断", body: "data: {\"choices\":[]}\n\n", wantProtocol: 1},
		{name: "DONE事件缺少空行", body: "data: [DONE]\n", wantProtocol: 1},
		{name: "多data行不能拼成DONE", body: "data: [DO\ndata: NE]\n\n", wantProtocol: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			stats := &counters{statuses: map[string]int64{}}
			var jobMu sync.Mutex
			var jobs []jobRef
			o := options{mode: "stream", timeout: time.Second}
			client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			if !doRequest(client, srv.URL, o, stats, &jobMu, &jobs) {
				t.Fatal("请求循环意外停止")
			}
			if stats.success != tc.wantSuccess || stats.protocol != tc.wantProtocol {
				t.Fatalf("success=%d protocol=%d，期望 %d / %d", stats.success, stats.protocol, tc.wantSuccess, tc.wantProtocol)
			}
			if stats.statuses["200"] != 1 {
				t.Fatalf("HTTP 状态计数应只记一次：%v", stats.statuses)
			}
			if got := len(stats.latency); got != int(tc.wantSuccess) {
				t.Fatalf("success_complete 延迟样本不应混入失败：%d", got)
			}
		})
	}
}

func TestCancelCountsFirstBodyByteAsExpectedCancellation(t *testing.T) {
	seenCancel := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(": first byte\n"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(seenCancel)
	}))
	defer srv.Close()
	stats := &counters{statuses: map[string]int64{}}
	var jobMu sync.Mutex
	var jobs []jobRef
	if !doRequest(&http.Client{}, srv.URL, options{mode: "cancel", timeout: time.Second}, stats, &jobMu, &jobs) {
		t.Fatal("请求循环意外停止")
	}
	if stats.canceled != 1 || stats.transport != 0 || stats.protocol != 0 {
		t.Fatalf("取消分类错误: canceled=%d transport=%d protocol=%d", stats.canceled, stats.transport, stats.protocol)
	}
	select {
	case <-seenCancel:
	case <-time.After(time.Second):
		t.Fatal("响应 body 字节后没有立即取消请求")
	}
}

func TestJobsCompletedIsNotTerminalUntilDone(t *testing.T) {
	var mu sync.Mutex
	reads := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reads++
		n := reads
		mu.Unlock()
		state := "completed"
		if n > 1 {
			state = "done"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "job-1", "state": state, "updated_at": time.Now().UTC()})
	}))
	defer srv.Close()
	s := &counters{statuses: map[string]int64{}, pollStatuses: map[string]int64{}}
	started := time.Now().Add(-20 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	pollJob(ctx, srv.Client(), srv.URL, options{timeout: time.Second}, s, jobRef{ID: "job-1", Started: started})
	if s.jobCompleted != 1 || s.jobFailed != 0 {
		t.Fatalf("done 终态未记录: completed=%d failed=%d", s.jobCompleted, s.jobFailed)
	}
	if reads < 2 {
		t.Fatalf("completed 应继续轮询，当前请求数=%d", reads)
	}
}

func TestReportKeepsJobsAndLatencyCategoriesSeparate(t *testing.T) {
	s := &counters{statuses: map[string]int64{}, pollStatuses: map[string]int64{}}
	s.addSuccess(10*time.Millisecond, 2, 200)
	s.addRejected(1 * time.Millisecond)
	s.addAccepted(3 * time.Millisecond)
	s.addJobTerminal(true, 40*time.Millisecond)
	r := s.makeReport(options{mode: "jobs", concurrency: 2, duration: time.Second}, 1, 2)
	if r.SuccessRPS != 0.5 {
		t.Fatalf("jobs success_rps 应按完成任务/total_seconds 计算，得 %v", r.SuccessRPS)
	}
	if r.AcceptedRPS != 1 {
		t.Fatalf("accepted_rps 应单独按 load_seconds 计算，得 %v", r.AcceptedRPS)
	}
	if strings.Contains(strings.TrimSpace(mustJSON(t, r)), "expected_canceled\":null") {
		t.Fatal("报告编码异常")
	}
	if r.LatencyMS["success_complete"].(map[string]float64)["p50"] != 10 {
		t.Fatal("503 等拒绝延迟污染了成功完成延迟")
	}
	if r.HTTPStatusCounts == nil {
		t.Fatal("缺少 HTTP 状态计数")
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRedirectIsReturnedAsUnexpectedStatus(t *testing.T) {
	called := false
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", destination.URL)
		w.WriteHeader(http.StatusFound)
	}))
	defer redirect.Close()
	stats := &counters{statuses: map[string]int64{}}
	var jobMu sync.Mutex
	var jobs []jobRef
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	_ = doRequest(client, redirect.URL, options{mode: "unary", timeout: time.Second}, stats, &jobMu, &jobs)
	if called {
		t.Fatal("HTTP 客户端跟随了 redirect")
	}
	if stats.protocol != 1 {
		t.Fatalf("redirect 应作为意外 HTTP 状态计错误：%d", stats.protocol)
	}
}
