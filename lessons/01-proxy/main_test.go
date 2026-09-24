package main

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const testAPIKey = "upstream-test-key"

func testGateway(t *testing.T, upstream *httptest.Server) *httptest.Server {
	t.Helper()
	base, err := url.Parse(upstream.URL + "/v1")
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := upstream.Client().Transport.(http.RoundTripper)
	if !ok {
		t.Fatal("upstream server has no HTTP transport")
	}
	return httptest.NewServer(newGateway(base, testAPIKey, transport))
}

func TestProxyStreamsBeforeUpstreamCompletesAndUsesConfiguredAuth(t *testing.T) {
	started := make(chan struct{})
	finished := make(chan struct{})
	release := make(chan struct{})
	var gotPath, gotAuthorization string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuthorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		close(started)
		<-release
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		close(finished)
	}))
	defer upstream.Close()
	proxy := testGateway(t, upstream)
	defer proxy.Close()

	request, err := http.NewRequest(http.MethodPost, proxy.URL+chatPath, strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer downstream-user-secret")
	request.Header.Set("Content-Type", "application/json")
	response, err := proxy.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not begin streaming")
	}

	first, err := bufio.NewReader(response.Body).ReadString('\n')
	if err != nil {
		t.Fatalf("reading first streamed line: %v", err)
	}
	if first != "data: first\n" {
		t.Fatalf("first line = %q, want first SSE event", first)
	}
	select {
	case <-finished:
		t.Fatal("upstream finished before the first chunk was observed downstream")
	default:
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("upstream path = %q", gotPath)
	}
	if gotAuthorization != "Bearer "+testAPIKey {
		t.Errorf("upstream Authorization = %q, want configured gateway key", gotAuthorization)
	}

	close(release)
	remaining, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(remaining), "data: [DONE]") {
		t.Errorf("remaining stream = %q, missing terminal event", remaining)
	}
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not finish after release")
	}
}

func TestClientDisconnectCancelsUpstream(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		close(started)
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-time.After(5 * time.Second):
			return
		}
	}))
	defer upstream.Close()
	proxy := testGateway(t, upstream)
	defer proxy.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, proxy.URL+chatPath, strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer downstream-secret")
	type result struct {
		response *http.Response
		err      error
	}
	done := make(chan result, 1)
	go func() {
		response, err := proxy.Client().Do(request)
		done <- result{response: response, err: err}
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not reach upstream")
	}
	var got result
	select {
	case got = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream response headers did not arrive")
	}
	if got.err != nil {
		t.Fatal(got.err)
	}
	if got.response == nil {
		t.Fatal("stream response is nil")
	}
	if first, err := bufio.NewReader(got.response.Body).ReadString('\n'); err != nil || first != "data: first\n" {
		t.Fatalf("first stream line = %q, err = %v", first, err)
	}
	cancel()
	_ = got.response.Body.Close()
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request context was not canceled")
	}
}

func TestRedirectIsRejectedWithoutLeakingDetails(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://provider.invalid/private?key="+testAPIKey, http.StatusFound)
	}))
	defer upstream.Close()
	proxy := testGateway(t, upstream)
	defer proxy.Close()

	response, err := proxy.Client().Post(proxy.URL+chatPath, "application/json", strings.NewReader(`{"model":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", response.StatusCode)
	}
	if strings.Contains(string(body), testAPIKey) || strings.Contains(response.Header.Get("Location"), "provider.invalid") {
		t.Fatalf("upstream redirect details leaked: status=%d location=%q body=%q", response.StatusCode, response.Header.Get("Location"), body)
	}
}

func TestOnlyChatCompletionsPostIsAccepted(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("unsupported route reached upstream")
	}))
	defer upstream.Close()
	proxy := testGateway(t, upstream)
	defer proxy.Close()

	for _, test := range []struct {
		method, path string
		wantStatus   int
	}{
		{http.MethodGet, chatPath, http.StatusMethodNotAllowed},
		{http.MethodPost, "/v1/embeddings", http.StatusNotFound},
	} {
		request, err := http.NewRequest(test.method, proxy.URL+test.path, strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		response, err := proxy.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != test.wantStatus {
			t.Errorf("%s %s status = %d, want %d", test.method, test.path, response.StatusCode, test.wantStatus)
		}
	}
}

func TestRequestBodyIsBounded(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("oversized request reached upstream")
	}))
	defer upstream.Close()
	proxy := testGateway(t, upstream)
	defer proxy.Close()

	request, err := http.NewRequest(http.MethodPost, proxy.URL+chatPath, strings.NewReader(strings.Repeat("x", maxBodyBytes+1)))
	if err != nil {
		t.Fatal(err)
	}
	response, err := proxy.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", response.StatusCode)
	}
}
