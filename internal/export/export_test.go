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
