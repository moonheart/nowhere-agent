package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// IM-bot delivery (domestic office-messaging integration): when a run-
// completion target points at a DingTalk / WeCom / Feishu group-bot webhook,
// the notifier formats the payload for that platform's schema instead of the
// generic JSON. All three are plain HTTPS POSTs to operator-created bot
// webhook URLs; DingTalk's optional 加签 (signing) mode is supported via a
// <secret> fragment on the URL:
//
//	https://oapi.dingtalk.com/robot/send?access_token=xxx#<secret>
//
// The URL host selects the platform:
//
//	oapi.dingtalk.com       → DingTalk  (加签 via #secret)
//	qyapi.weixin.qq.com     → WeCom     (key query param)
//	open.feishu.cn          → Feishu    (hook path id)

// imBotHosts are the group-bot endpoints the notifier special-cases. Tests
// replace the notifier's host map to route deliveries at a local server.
var imBotHosts = map[string]bool{
	"oapi.dingtalk.com":   true,
	"qyapi.weixin.qq.com": true,
	"open.feishu.cn":      true,
}

// isIMBotURL reports whether raw targets one of the notifier's bot hosts.
func (n *Notifier) isIMBotURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return n.imHosts[u.Hostname()]
}

// imPayloadFor renders the platform-specific request body for a run-completion
// summary. A host in the notifier's map that matches none of the named
// platforms falls back to the generic text schema (this is what test-injected
// hosts exercise); a host outside the map is refused.
func (n *Notifier) imPayloadFor(raw string, payload RunCompletedPayload) ([]byte, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	host := u.Hostname()
	if !n.imHosts[host] {
		return nil, fmt.Errorf("not an IM bot URL: %s", raw)
	}
	text := imSummary(payload)
	switch host {
	case "oapi.dingtalk.com":
		return json.Marshal(map[string]any{
			"msgtype": "text",
			"text":    map[string]string{"content": text},
		})
	case "qyapi.weixin.qq.com":
		return json.Marshal(map[string]any{
			"msgtype": "text",
			"text":    map[string]string{"content": text},
		})
	case "open.feishu.cn":
		return json.Marshal(map[string]any{
			"msg_type": "text",
			"content":  map[string]string{"text": text},
		})
	default:
		return json.Marshal(map[string]any{
			"msgtype": "text",
			"text":    map[string]string{"content": text},
		})
	}
}

// imSummary renders the human-readable run-completion line for IM channels.
func imSummary(p RunCompletedPayload) string {
	line := fmt.Sprintf("Agent 任务完成(任务 %s):%s", p.RunID, p.Status)
	if p.Summary != "" {
		line += "\n" + p.Summary
	}
	if len(line) > 1800 {
		r := []rune(line)
		line = string(r[:1800]) + "…"
	}
	return line
}

// imSendURL rewrites a bot URL for DingTalk 加签 mode: a #secret fragment
// becomes timestamp + sign query parameters (only DingTalk URLs carry the
// fragment convention).
func imSendURL(raw string, now time.Time) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Fragment == "" {
		return raw, nil
	}
	ts := now.UnixMilli()
	signStr := fmt.Sprintf("%d\n%s", ts, u.Fragment)
	m := hmac.New(sha256.New, []byte(u.Fragment))
	m.Write([]byte(signStr))
	sign := url.QueryEscape(base64.StdEncoding.EncodeToString(m.Sum(nil)))
	u.Fragment = ""
	q := u.Query()
	q.Set("timestamp", fmt.Sprintf("%d", ts))
	q.Set("sign", sign)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// deliverIM posts the platform-formatted payload to the bot webhook.
func (n *Notifier) deliverIM(ctx context.Context, raw string, payload RunCompletedPayload) error {
	body, err := n.imPayloadFor(raw, payload)
	if err != nil {
		return err
	}
	target, err := imSendURL(raw, time.Now())
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "nowhere-agent-webhook/1")
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return errUnexpectedStatus(resp.StatusCode)
}
