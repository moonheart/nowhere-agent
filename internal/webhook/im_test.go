package webhook

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

// captureBody records the request body a test server received.
type captureBody struct {
	raw []byte
	ch  chan struct{}
}

func newCapture() *captureBody {
	return &captureBody{ch: make(chan struct{}, 1)}
}

func (c *captureBody) handler(w http.ResponseWriter, r *http.Request) {
	c.raw, _ = io.ReadAll(r.Body)
	c.ch <- struct{}{}
	w.WriteHeader(http.StatusOK)
}

func (c *captureBody) wait(t *testing.T) map[string]any {
	t.Helper()
	select {
	case <-c.ch:
	case <-time.After(2 * time.Second):
		t.Fatal("bot webhook never received the delivery")
	}
	var out map[string]any
	if err := json.Unmarshal(c.raw, &out); err != nil {
		t.Fatalf("payload not json: %v", err)
	}
	return out
}

func imPayload() RunCompletedPayload {
	return RunCompletedPayload{
		Event: "run.completed", RunID: "run-1", Status: "done",
		Summary: "已完成工单 #123 的分析。",
	}
}

// rewriteTransport dials the test server while keeping the bot's real
// hostname in the URL, so the delivery path exercises the exact production
// branch (schema + signing host checks) without real DNS.
type rewriteTransport struct{ target string }

func (t rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Host = t.target
	clone.URL.Scheme = "http" // the test server speaks plain HTTP
	clone.Host = req.URL.Host
	return http.DefaultTransport.RoundTrip(clone)
}

// imNotifier builds a notifier whose IM host set is the REAL bot hosts, with
// the HTTP transport rewritten to the loopback test server.
func imNotifier(t *testing.T, srv *httptest.Server) *Notifier {
	t.Helper()
	target := strings.TrimPrefix(srv.URL, "http://")
	g := testGuard(t, []string{"127.0.0.0/8"}, nil)
	n := New(Options{SSRF: g, Timeout: 2 * time.Second, Logger: testLogger(t)})
	n.client.Transport = rewriteTransport{target: target}
	return n
}

// TestDingTalkBotFormat pins the DingTalk schema (and exercises the 加签
// fragment rewrite through the delivery path).
func TestDingTalkBotFormat(t *testing.T) {
	cap := newCapture()
	srv := httptest.NewServer(http.HandlerFunc(cap.handler))
	defer srv.Close()
	n := imNotifier(t, srv)

	if err := n.Deliver(context.Background(), "https://oapi.dingtalk.com/robot/send?access_token=abc#mysecret", imPayload()); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	body := cap.wait(t)
	if body["msgtype"] != "text" {
		t.Fatalf("msgtype = %v, want text", body["msgtype"])
	}
	text, ok := body["text"].(map[string]any)
	if !ok || !strings.Contains(text["content"].(string), "工单 #123") {
		t.Fatalf("text payload missing summary: %v", body)
	}
}

// TestWeComBotFormat pins the WeCom group-bot schema.
func TestWeComBotFormat(t *testing.T) {
	cap := newCapture()
	srv := httptest.NewServer(http.HandlerFunc(cap.handler))
	defer srv.Close()
	n := imNotifier(t, srv)

	if err := n.Deliver(context.Background(), "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=k1", imPayload()); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	body := cap.wait(t)
	if body["msgtype"] != "text" {
		t.Fatalf("msgtype = %v, want text", body["msgtype"])
	}
}

// TestFeishuBotFormat pins the Feishu custom-bot schema.
func TestFeishuBotFormat(t *testing.T) {
	cap := newCapture()
	srv := httptest.NewServer(http.HandlerFunc(cap.handler))
	defer srv.Close()
	n := imNotifier(t, srv)

	if err := n.Deliver(context.Background(), "https://open.feishu.cn/open-apis/bot/v2/hook/x", imPayload()); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	body := cap.wait(t)
	if body["msg_type"] != "text" {
		t.Fatalf("msg_type = %v, want text", body["msg_type"])
	}
	content, ok := body["content"].(map[string]any)
	if !ok || content["text"] == "" {
		t.Fatalf("content payload missing text: %v", body)
	}
}

// TestIMPayloadSchemas pins the exact per-platform payloads at the formatter
// level (production hostnames, no server needed).
func TestIMPayloadSchemas(t *testing.T) {
	n := New(Options{Logger: testLogger(t)})
	ding, err := n.imPayloadFor("https://oapi.dingtalk.com/robot/send?access_token=a", imPayload())
	if err != nil || !strings.Contains(string(ding), `"msgtype":"text"`) {
		t.Fatalf("dingtalk payload: %s %v", ding, err)
	}
	feishu, err := n.imPayloadFor("https://open.feishu.cn/open-apis/bot/v2/hook/x", imPayload())
	if err != nil || !strings.Contains(string(feishu), `"msg_type":"text"`) {
		t.Fatalf("feishu payload: %s %v", feishu, err)
	}
	if _, err := n.imPayloadFor("https://hooks.example.com/x", imPayload()); err == nil {
		t.Fatal("non-bot host accepted")
	}
}

func TestIMSendURLDingTalkSign(t *testing.T) {
	raw := "https://oapi.dingtalk.com/robot/send?access_token=abc#mysecret"
	got, err := imSendURL(raw, time.UnixMilli(1700000000000))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "timestamp=1700000000000") || !strings.Contains(got, "sign=") {
		t.Fatalf("sign query missing: %s", got)
	}
	if strings.Contains(got, "mysecret") {
		t.Fatalf("secret leaked into the URL: %s", got)
	}
	// Non-dingtalk or fragment-less URLs pass through untouched.
	for _, u := range []string{
		"https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=k",
		"https://oapi.dingtalk.com/robot/send?access_token=abc",
	} {
		if got, err := imSendURL(u, time.Now()); err != nil || got != u {
			t.Errorf("imSendURL(%q) = %q, %v", u, got, err)
		}
	}
}

func TestIMBotHosts(t *testing.T) {
	n := New(Options{Logger: testLogger(t)})
	for _, u := range []string{
		"https://oapi.dingtalk.com/robot/send?access_token=x",
		"https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=x",
		"https://open.feishu.cn/open-apis/bot/v2/hook/x",
	} {
		if !n.isIMBotURL(u) {
			t.Errorf("isIMBotURL(%q) = false, want true", u)
		}
	}
	for _, u := range []string{"https://hooks.example.com/x", "http://oapi.dingtalk.com.evil/x"} {
		if n.isIMBotURL(u) {
			t.Errorf("isIMBotURL(%q) = true, want false", u)
		}
	}
}
