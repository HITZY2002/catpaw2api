package upstream

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPollAssistantUsesLatestRoundAndWaitsForCompletion(t *testing.T) {
	oldDirect := DirectHost
	defer func() { DirectHost = oldDirect }()

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		finished := calls >= 2
		current := "partial"
		if finished {
			current = "new answer"
		}
		_, _ = fmt.Fprintf(w, `{"unifyCode":0,"code":0,"msg":"成功","data":{"items":[`+
			`{"type":"user","roundId":1,"status":1,"finished":true,"totalUsage":{"total_tokens":10},"content":[{"type":"text","text":"q1"}]},`+
			`{"type":"assistant","roundId":1,"status":1,"finished":true,"content":[{"type":"text","text":"old answer"}]},`+
			`{"type":"user","roundId":2,"status":1,"finished":true,"totalUsage":{"total_tokens":20},"content":[{"type":"text","text":"q2"}]},`+
			`{"type":"assistant","roundId":2,"status":1,"finished":%t,"content":[{"type":"text","text":%q}]}`+
			`]},"success":true}`, finished, current)
	}))
	defer srv.Close()
	DirectHost = srv.URL

	c := New(2 * time.Second)
	res, err := c.PollAssistant(context.Background(), "tok", "uid", "conv", PollOpts{
		Interval: 5 * time.Millisecond,
		Timeout:  time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls < 2 {
		t.Fatalf("poll count=%d, expected at least 2", calls)
	}
	if res.Content != "new answer" {
		t.Fatalf("content=%q, want latest completed round", res.Content)
	}
	if got, ok := res.Usage["total_tokens"].(float64); !ok || got != 20 {
		t.Fatalf("usage=%v, want latest user usage", res.Usage)
	}
	if res.Finish != "stop" {
		t.Fatalf("finish=%q", res.Finish)
	}
}

func TestPollAssistantPrefersRoundMetadataOverArrayOrder(t *testing.T) {
	oldDirect := DirectHost
	defer func() { DirectHost = oldDirect }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Newer round intentionally appears before an older round to prove that
		// roundId, not "last array element", determines recency when available.
		_, _ = w.Write([]byte(`{"unifyCode":0,"code":0,"msg":"成功","data":{"items":[` +
			`{"type":"user","roundId":2,"status":1,"finished":true,"totalUsage":{"total_tokens":22},"content":[{"type":"text","text":"q2"}]},` +
			`{"type":"assistant","roundId":2,"status":1,"finished":true,"content":[{"type":"text","text":"round two"}]},` +
			`{"type":"user","roundId":1,"status":1,"finished":true,"totalUsage":{"total_tokens":11},"content":[{"type":"text","text":"q1"}]},` +
			`{"type":"assistant","roundId":1,"status":1,"finished":true,"content":[{"type":"text","text":"round one"}]}` +
			`]},"success":true}`))
	}))
	defer srv.Close()
	DirectHost = srv.URL

	c := New(2 * time.Second)
	res, err := c.PollAssistant(context.Background(), "tok", "uid", "conv", PollOpts{
		Interval: 5 * time.Millisecond,
		Timeout:  time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "round two" {
		t.Fatalf("content=%q, want round two", res.Content)
	}
	if got, ok := res.Usage["total_tokens"].(float64); !ok || got != 22 {
		t.Fatalf("usage=%v, want round two usage", res.Usage)
	}
}

func TestPollAssistantDoesNotReturnPartialLatestRound(t *testing.T) {
	oldDirect := DirectHost
	defer func() { DirectHost = oldDirect }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"unifyCode":0,"code":0,"msg":"成功","data":{"items":[` +
			`{"type":"assistant","roundId":1,"status":1,"finished":true,"content":[{"type":"text","text":"old"}]},` +
			`{"type":"assistant","roundId":2,"status":1,"finished":false,"content":[{"type":"text","text":"partial"}]}` +
			`]},"success":true}`))
	}))
	defer srv.Close()
	DirectHost = srv.URL

	c := New(2 * time.Second)
	if _, err := c.PollAssistant(context.Background(), "tok", "uid", "conv", PollOpts{
		Interval: 5 * time.Millisecond,
		Timeout:  40 * time.Millisecond,
	}); err == nil {
		t.Fatal("expected timeout while newest assistant is unfinished")
	}
}
