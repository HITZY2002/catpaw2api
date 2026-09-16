package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 实测（2026-09-16）：model 与 modelType 都不发时上游返回
// data.success=false / "model 与 modelType 不能同时为空"，
// 旧实现把 auto 也跳过，导致默认模型完全不可用。
func TestSendMessageAlwaysSendsModel(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"msg":"","data":{"conversationId":"conv-1","success":true}}`)
	}))
	defer srv.Close()

	c := New(5 * time.Second)
	c.nocodeBase = srv.URL
	for _, model := range []string{"auto", "", "glm-5.3-flash"} {
		got = nil
		if _, err := c.SendMessage(context.Background(), "tok", "desk-1", "hi", model); err != nil {
			t.Fatalf("model=%q: %v", model, err)
		}
		if got["model"] == nil || got["model"] == "" {
			t.Fatalf("model=%q 时未发送 model 字段，上游会拒绝：%v", model, got)
		}
	}
}

// data.success=false 时不能只报 "empty conversationId"，要把上游原因带出来。
func TestSendMessageSurfacesDataFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"msg":"","data":{"conversationId":null,"success":false,"errorCode":500,"errorMessage":"model 与 modelType 不能同时为空"}}`)
	}))
	defer srv.Close()

	c := New(5 * time.Second)
	c.nocodeBase = srv.URL
	_, err := c.SendMessage(context.Background(), "tok", "desk-1", "hi", "auto")
	if err == nil {
		t.Fatal("success=false 应返回错误")
	}
	if !strings.Contains(err.Error(), "modelType") {
		t.Fatalf("错误信息应包含上游原因，got=%v", err)
	}
}
