package export

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/memory"
	"nowhere-agent/internal/session"
)

// memMessageStore is a minimal MessageStore for export tests.
type memMessageStore struct {
	sessions map[string][]session.StoredMessage
}

func (m *memMessageStore) AppendMessage(ctx context.Context, msg session.StoredMessage) (session.StoredMessage, error) {
	m.sessions[msg.SessionID] = append(m.sessions[msg.SessionID], msg)
	return msg, nil
}
func (m *memMessageStore) MessagesFor(ctx context.Context, sessionID string) ([]session.StoredMessage, error) {
	return m.sessions[sessionID], nil
}
func (m *memMessageStore) MessagesAfter(ctx context.Context, sessionID string, afterID int64) ([]session.StoredMessage, error) {
	var out []session.StoredMessage
	for _, msg := range m.sessions[sessionID] {
		if msg.ID > afterID {
			out = append(out, msg)
		}
	}
	return out, nil
}
func (m *memMessageStore) MessagesPage(ctx context.Context, sessionID string, afterID int64, limit int) ([]session.StoredMessage, error) {
	var out []session.StoredMessage
	for _, msg := range m.sessions[sessionID] {
		if msg.ID > afterID {
			out = append(out, msg)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}
func (m *memMessageStore) SetMessageMetadata(ctx context.Context, id int64, metadata json.RawMessage) error {
	for _, msgs := range m.sessions {
		for i := range msgs {
			if msgs[i].ID == id {
				msgs[i].Metadata = metadata
			}
		}
	}
	return nil
}
func (m *memMessageStore) LastAssistantText(ctx context.Context, sessionID string, limit int) (string, error) {
	msgs := m.sessions[sessionID]
	if limit <= 0 {
		return "", nil
	}
	for i := len(msgs) - 1; i >= 0 && limit > 0; i-- {
		if msgs[i].Role != "assistant" {
			continue
		}
		limit--
		var b strings.Builder
		for _, blk := range msgs[i].Content {
			if blk.Type == "text" {
				b.WriteString(blk.Text)
			}
		}
		if s := strings.TrimSpace(b.String()); s != "" {
			return s, nil
		}
	}
	return "", nil
}

func TestWriteProducesCompleteJSON(t *testing.T) {
	msgs := &memMessageStore{sessions: map[string][]session.StoredMessage{
		"s1": {
			{ID: 1, SessionID: "s1", Seq: 1, Role: "user", Content: nil},
			{ID: 2, SessionID: "s1", Seq: 2, Role: "assistant", Content: nil},
		},
	}}
	mem := memory.NewMemPort()
	if _, err := mem.Store(context.Background(), memory.Memory{
		Scope:   identity.UserScope("u1"),
		Kind:    "fact",
		Content: "企业A的核心系统是SAP",
	}); err != nil {
		t.Fatal(err)
	}
	svc := New(nil, msgs, mem, nil, nil) // nil db → sessions section stays empty (test path)

	var buf strings.Builder
	u := identity.User{ID: "u1", Email: "a@b.cn", DisplayName: "张三", CreatedAt: time.Now()}
	if err := svc.Write(context.Background(), &buf, u); err != nil {
		t.Fatalf("Write: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(buf.String()), &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	if doc["user"].(map[string]any)["email"] != "a@b.cn" {
		t.Errorf("user section missing email: %v", doc["user"])
	}
	sessions := doc["sessions"].([]any)
	if len(sessions) != 0 {
		t.Errorf("sessions = %v, want none", sessions)
	}
	mems := doc["memories"].([]any)
	if len(mems) != 1 || mems[0].(map[string]any)["content"] != "企业A的核心系统是SAP" {
		t.Errorf("memories = %v", mems)
	}
	// Optional sections are omitted when their source is nil.
	if _, ok := doc["uploads"]; ok {
		t.Error("uploads section should be absent without an upload source")
	}
}

// messages1N builds n StoredMessages with ascending ids/seqs (the order the
// stores return).
func messages1N(n int) []session.StoredMessage {
	out := make([]session.StoredMessage, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, session.StoredMessage{ID: int64(i), SessionID: "s1", Seq: i, Role: "assistant"})
	}
	return out
}

// TestWriteSessionStreamsMessagePages pins the keyset streaming: a session
// with more messages than one page must export every message, in seq order,
// spliced into ONE valid JSON array — the document shape is identical to the
// old full-load encoding, only the working set is bounded.
func TestWriteSessionStreamsMessagePages(t *testing.T) {
	total := messagePageSize + 3
	svc := New(nil, &memMessageStore{sessions: map[string][]session.StoredMessage{
		"s1": messages1N(total),
	}}, nil, nil, nil)

	var buf strings.Builder
	if err := svc.writeSession(context.Background(), &buf, json.NewEncoder(&buf),
		sessionMeta{ID: "s1", Title: "t", Status: "active"}); err != nil {
		t.Fatalf("writeSession: %v", err)
	}
	var sess struct {
		ID       string `json:"id"`
		Messages []struct {
			ID   int64  `json:"id"`
			Seq  int    `json:"seq"`
			Role string `json:"role"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(buf.String()), &sess); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	if sess.ID != "s1" {
		t.Errorf("session id = %q, want s1", sess.ID)
	}
	if len(sess.Messages) != total {
		t.Fatalf("messages = %d, want %d (all pages must be exported)", len(sess.Messages), total)
	}
	for i, m := range sess.Messages {
		if want := int64(i + 1); m.ID != want || m.Seq != i+1 {
			t.Fatalf("message[%d] = id %d seq %d, want id/seq %d (seq order across pages)", i, m.ID, m.Seq, want)
		}
	}
}

// TestWriteSessionNullMessagesWhenEmpty pins the shape of a session without
// messages: "messages":null, exactly as the pre-streaming encoding produced.
func TestWriteSessionNullMessagesWhenEmpty(t *testing.T) {
	svc := New(nil, &memMessageStore{sessions: map[string][]session.StoredMessage{
		"s1": {},
	}}, nil, nil, nil)

	var buf strings.Builder
	if err := svc.writeSession(context.Background(), &buf, json.NewEncoder(&buf), sessionMeta{ID: "s1"}); err != nil {
		t.Fatalf("writeSession: %v", err)
	}
	body := buf.String()
	if !strings.Contains(body, `"messages":null`) {
		t.Errorf("empty session rendered %q, want \"messages\":null", body)
	}
	if !strings.HasSuffix(body, "}") {
		t.Errorf("session object not closed: %q", body)
	}
}
