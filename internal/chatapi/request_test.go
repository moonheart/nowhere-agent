package chatapi

import (
	"encoding/json"
	"testing"

	"nowhere-agent/internal/provider"
)

func TestToHistoryMixedTextAndImage(t *testing.T) {
	req := dataStreamRequest{
		Messages: []incomingMessage{{
			Role: "user",
			Parts: []incomingPart{
				{Type: "text", Text: "look at this"},
				{Type: "image", MediaType: "image/webp", Path: "img/a.webp"},
			},
		}},
	}
	history := toHistory(req)
	if len(history) != 1 {
		t.Fatalf("history = %d messages, want 1", len(history))
	}
	blocks := history[0].Content
	if len(blocks) != 2 {
		t.Fatalf("content = %d blocks, want 2 (text + image)", len(blocks))
	}
	if blocks[0].Type != provider.BlockText || blocks[0].Text != "look at this" {
		t.Errorf("block[0] = %+v, want the text block", blocks[0])
	}
	if blocks[1].Type != provider.BlockImage {
		t.Fatalf("block[1].Type = %q, want image", blocks[1].Type)
	}
	if blocks[1].ImagePath != "img/a.webp" || blocks[1].MediaType != "image/webp" {
		t.Errorf("image block = %+v, want path+mediaType", blocks[1])
	}
}

func TestToHistoryImageOnlyMessage(t *testing.T) {
	req := dataStreamRequest{
		Messages: []incomingMessage{{
			Role: "user",
			Parts: []incomingPart{
				{Type: "image", Path: "img/only.webp"},
			},
		}},
	}
	history := toHistory(req)
	if len(history) != 1 {
		t.Fatalf("history = %d messages, want 1", len(history))
	}
	blocks := history[0].Content
	if len(blocks) != 1 || blocks[0].Type != provider.BlockImage {
		t.Fatalf("blocks = %+v, want a single image block", blocks)
	}
	// MediaType defaults to WebP when omitted.
	if blocks[0].MediaType != "image/webp" {
		t.Errorf("mediaType = %q, want image/webp default", blocks[0].MediaType)
	}
}

func TestToHistoryContentArrayImageParts(t *testing.T) {
	// content may arrive as a JSON array of parts (AI-SDK form).
	req := dataStreamRequest{
		Messages: []incomingMessage{{
			Role:    "user",
			Content: []byte(`[{"type":"text","text":"hi"},{"type":"image","path":"img/b.webp","mediaType":"image/webp"}]`),
		}},
	}
	history := toHistory(req)
	if len(history) != 1 {
		t.Fatalf("history = %d messages, want 1", len(history))
	}
	blocks := history[0].Content
	if len(blocks) != 2 || blocks[1].Type != provider.BlockImage || blocks[1].ImagePath != "img/b.webp" {
		t.Errorf("blocks = %+v, want text+image parsed from content array", blocks)
	}
}

func TestToHistoryPlainStringStillWorks(t *testing.T) {
	req := dataStreamRequest{
		Messages: []incomingMessage{{
			Role:    "user",
			Content: []byte(`"just text"`),
		}},
	}
	history := toHistory(req)
	if len(history) != 1 || len(history[0].Content) != 1 || history[0].Content[0].Text != "just text" {
		t.Fatalf("history = %+v, want a single text block", history)
	}
}

func TestUserTurnBlocksLastMessageOnly(t *testing.T) {
	req := dataStreamRequest{
		Messages: []incomingMessage{
			{Role: "user", Parts: []incomingPart{{Type: "text", Text: "earlier"}}},
			{Role: "user", Parts: []incomingPart{
				{Type: "text", Text: "now"},
				{Type: "image", Path: "img/last.webp"},
			}},
		},
	}
	blocks := userTurnBlocks(req)
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d, want 2 from the LAST user message", len(blocks))
	}
	if blocks[0].Text != "now" {
		t.Errorf("block[0].Text = %q, want the last message's text", blocks[0].Text)
	}
}

func TestUserTurnBlocksNone(t *testing.T) {
	req := dataStreamRequest{Messages: []incomingMessage{{Role: "assistant", Parts: []incomingPart{{Type: "text", Text: "hi"}}}}}
	if blocks := userTurnBlocks(req); blocks != nil {
		t.Errorf("blocks = %v, want nil with no user message", blocks)
	}
}

func TestToHistoryTopLevelImagesAppendToLastUserMessage(t *testing.T) {
	req := dataStreamRequest{
		Messages: []incomingMessage{{Role: "user", Parts: []incomingPart{{Type: "text", Text: "what is this"}}}},
		Images:   []incomingImagePart{{MediaType: "image/webp", Path: "img/a.webp"}},
	}
	history := toHistory(req)
	if len(history) != 1 {
		t.Fatalf("history = %d messages, want 1", len(history))
	}
	blocks := history[0].Content
	if len(blocks) != 2 {
		t.Fatalf("content = %d blocks, want text + appended image", len(blocks))
	}
	if blocks[0].Type != provider.BlockText || blocks[0].Text != "what is this" {
		t.Errorf("block[0] = %+v, want the text block", blocks[0])
	}
	if blocks[1].Type != provider.BlockImage || blocks[1].ImagePath != "img/a.webp" || blocks[1].MediaType != "image/webp" {
		t.Errorf("block[1] = %+v, want the appended image block", blocks[1])
	}
}

func TestToHistoryTopLevelImagesOnly(t *testing.T) {
	req := dataStreamRequest{Images: []incomingImagePart{{Path: "img/only.webp"}}}
	history := toHistory(req)
	if len(history) != 1 || len(history[0].Content) != 1 {
		t.Fatalf("history = %+v, want a single image-only user message", history)
	}
	img := history[0].Content[0]
	if img.Type != provider.BlockImage || img.ImagePath != "img/only.webp" {
		t.Errorf("image block = %+v", img)
	}
	if img.MediaType != "image/webp" {
		t.Errorf("mediaType = %q, want image/webp default", img.MediaType)
	}
}

func TestUserTurnBlocksTopLevelImagesAppend(t *testing.T) {
	req := dataStreamRequest{
		Messages: []incomingMessage{
			{Role: "assistant", Parts: []incomingPart{{Type: "text", Text: "hi"}}},
			{Role: "user", Parts: []incomingPart{{Type: "text", Text: "look"}}},
		},
		Images: []incomingImagePart{{Path: "img/last.webp"}},
	}
	blocks := userTurnBlocks(req)
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d, want text + appended top-level image", len(blocks))
	}
	if blocks[1].Type != provider.BlockImage || blocks[1].ImagePath != "img/last.webp" {
		t.Errorf("block[1] = %+v, want the appended image", blocks[1])
	}
}

func TestUserTurnBlocksImagesWithEmptyText(t *testing.T) {
	// An image-only turn: the last user message carries no text, but the
	// top-level images still produce a durable user message.
	req := dataStreamRequest{
		Messages: []incomingMessage{{Role: "user", Parts: []incomingPart{{Type: "text", Text: ""}}}},
		Images:   []incomingImagePart{{Path: "img/blank.webp"}},
	}
	blocks := userTurnBlocks(req)
	if len(blocks) != 1 || blocks[0].Type != provider.BlockImage {
		t.Fatalf("blocks = %+v, want a single image block", blocks)
	}
}

func TestDataStreamRequestDecodesTopLevelImages(t *testing.T) {
	var req dataStreamRequest
	body := `{"messages":[{"role":"user","parts":[{"type":"text","text":"x"}]}],` +
		`"images":[{"path":"img/a.webp","mediaType":"image/webp"}]}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(req.Images) != 1 || req.Images[0].Path != "img/a.webp" || req.Images[0].MediaType != "image/webp" {
		t.Fatalf("images = %+v, want the decoded top-level image", req.Images)
	}
	// And an image-only body with a missing media type still defaults to WebP.
	if blocks := userTurnBlocks(req); len(blocks) != 2 || blocks[1].MediaType != "image/webp" {
		t.Errorf("blocks = %+v, want text + default-mediaType image", blocks)
	}
}
