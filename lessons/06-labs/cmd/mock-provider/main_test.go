package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMockFaultAndStream(t *testing.T) {
	s := httptest.NewServer(newMock(settings{status: 200, chunks: 3, chunkBytes: 4, failEvery: 2, failureStatus: 503, stallAfter: -1}))
	defer s.Close()
	for i := 1; i <= 2; i++ {
		res, err := http.Post(s.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"demo","stream":true}`))
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			if strings.Count(string(body), "data:") != 4 || !strings.Contains(string(body), "[DONE]") {
				t.Fatal(string(body))
			}
		} else if res.StatusCode != 503 {
			t.Fatal(res.StatusCode)
		}
	}
}
func TestMockStallObservesCancellation(t *testing.T) {
	s := httptest.NewServer(newMock(settings{status: 200, chunks: 10, chunkBytes: 4, stallAfter: 1}))
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, "POST", s.URL+"/v1/chat/completions", strings.NewReader(`{"stream":true}`))
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	var b [8]byte
	if _, err := res.Body.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	cancel()
	res.Body.Close()
	until := time.Now().Add(time.Second)
	for time.Now().Before(until) {
		res, err := http.Get(s.URL + "/stats")
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if strings.Contains(string(data), `"canceled":1`) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("mock did not release canceled stream")
}
