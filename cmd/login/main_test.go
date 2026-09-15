package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCallbackHandlerAcceptsMatchingState(t *testing.T) {
	ch := make(chan string, 1)
	h := callbackHandler("state-123", ch)
	req := httptest.NewRequest(http.MethodGet, "/callback?token=tok-abc&state=state-123", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	select {
	case got := <-ch:
		if got != "tok-abc" {
			t.Fatalf("token=%q", got)
		}
	default:
		t.Fatal("callback did not deliver token")
	}
}

func TestCallbackHandlerRejectsWrongOrMissingState(t *testing.T) {
	for _, rawURL := range []string{
		"/callback?token=tok-abc&state=wrong",
		"/callback?token=tok-abc",
	} {
		t.Run(rawURL, func(t *testing.T) {
			ch := make(chan string, 1)
			h := callbackHandler("state-123", ch)
			req := httptest.NewRequest(http.MethodGet, rawURL, nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
			select {
			case got := <-ch:
				t.Fatalf("unexpected token delivered: %q", got)
			default:
			}
		})
	}
}

func TestCallbackHandlerValidatesJSONState(t *testing.T) {
	ch := make(chan string, 1)
	h := callbackHandler("json-state", ch)
	req := httptest.NewRequest(http.MethodPost, "/callback", strings.NewReader(`{"token":"tok-json","state":"json-state"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := <-ch; got != "tok-json" {
		t.Fatalf("token=%q", got)
	}
}

func TestCallbackHandlerRejectsUnsupportedMethod(t *testing.T) {
	ch := make(chan string, 1)
	h := callbackHandler("state-123", ch)
	req := httptest.NewRequest(http.MethodPut, "/callback", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d", rr.Code)
	}
}
