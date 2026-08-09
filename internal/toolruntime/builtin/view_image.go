package builtin

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/toolruntime"
)

// ViewImageToolName is the tool the model calls to inspect an attached image
// through a dedicated vision model (image-input capability). The main model may
// lack native image support; this tool hands the image to a cheap vision-capable
// model and returns the description as text.
const ViewImageToolName = "view_image"

// viewImage is the built-in vision tool. It is bound per session: the resolver
// confines reads to that session's workspace, and the vision adapter + model are
// the configured VISION_* settings. RiskReadOnly: it makes no external change.
type viewImage struct {
	adapter  provider.Adapter
	model    string
	resolver provider.ImageResolver
}

// NewViewImage returns the view_image tool bound to one session. adapter is the
// vision provider, model the vision model name, and resolver resolves the
// session's image bytes by workspace-relative path.
func NewViewImage(adapter provider.Adapter, model string, resolver provider.ImageResolver) toolruntime.Tool {
	return viewImage{adapter: adapter, model: model, resolver: resolver}
}

func (viewImage) Name() string { return ViewImageToolName }
func (viewImage) Risk() toolruntime.Risk {
	return toolruntime.RiskReadOnly
}
func (viewImage) Timeout() time.Duration { return 60 * time.Second } // a vision round-trip

func (viewImage) Description() string {
	return "Inspect an attached image with a vision model and return what it shows. Use this when the " +
		"user has attached an image (the conversation contains a hint like '已附加图片') and you need to " +
		"see or read it: pass the image's workspace-relative path and an optional question. Returns the " +
		"vision model's description/answer as text."
}

// Schema: path (workspace-relative, from the attachment hint) + optional question.
func (viewImage) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Workspace-relative path of the attached image (e.g. the path named in the attachment hint).",
			},
			"question": map[string]any{
				"type":        "string",
				"description": "Optional specific question about the image; empty means 'describe the image'.",
			},
		},
		"required": []string{"path"},
	}
}

// Call resolves the image bytes, sends them with the question to the vision
// model, and returns the assembled text response.
func (t viewImage) Call(ctx context.Context, args map[string]any) (toolruntime.Result, error) {
	path, _ := args["path"].(string)
	if path == "" {
		return toolruntime.Result{Content: "view_image: 'path' is required", IsError: true}, nil
	}
	if t.adapter == nil || t.resolver == nil {
		return toolruntime.Result{Content: "view_image: vision model not configured", IsError: true}, nil
	}
	question, _ := args["question"].(string)

	data, err := t.resolver.ResolveImage(ctx, path)
	if err != nil {
		return toolruntime.Result{
			Content: fmt.Sprintf("view_image: cannot read image %q (missing file or outside the session workspace): %v", path, err),
			IsError: true,
		}, nil
	}

	system := "You are an image-understanding assistant. Describe the image concisely and answer the " +
		"user's question accurately; state what you cannot determine. Respond in the language of the question."
	if question == "" {
		question = "Describe this image in detail."
	}
	// Force image input: this tool exists specifically to send the image to a
	// vision model, whose name may not be in the capability table (self-hosted
	// vLLM deployments). Without the override the OpenAI adapter would degrade
	// the image block to a text reference and the model would answer blind.
	forceImage := true
	req := provider.Request{
		Model:      t.model,
		System:     system,
		MaxTokens:  2048,
		ImageInput: &forceImage,
		Messages: []provider.Message{{
			Role: provider.RoleUser,
			Content: []provider.Block{
				{Type: provider.BlockText, Text: question},
				{Type: provider.BlockImage, MediaType: "image/webp", ImagePath: path, ImageData: base64.StdEncoding.EncodeToString(data)},
			},
		}},
	}

	events, err := t.adapter.Stream(ctx, req)
	if err != nil {
		return toolruntime.Result{Content: fmt.Sprintf("view_image: vision model call failed: %v", err), IsError: true}, nil
	}
	text, err := collectText(ctx, events)
	if err != nil {
		return toolruntime.Result{Content: fmt.Sprintf("view_image: vision model stream failed: %v", err), IsError: true}, nil
	}
	if strings.TrimSpace(text) == "" {
		return toolruntime.Result{Content: "view_image: vision model returned no text", IsError: true}, nil
	}
	return toolruntime.Result{Content: text}, nil
}

// collectText drains a provider stream, concatenating text block deltas in
// order. It stops on ctx cancellation (run interrupted) and surfaces provider
// errors.
func collectText(ctx context.Context, events <-chan provider.Event) (string, error) {
	var b strings.Builder
	open := map[int]provider.BlockType{}
	for {
		select {
		case <-ctx.Done():
			return b.String(), ctx.Err()
		case ev, ok := <-events:
			if !ok {
				return b.String(), nil
			}
			switch ev.Type {
			case provider.EventError:
				return b.String(), ev.Err
			case provider.EventBlockStart:
				if ev.Block != nil {
					open[ev.Index] = ev.Block.Type
				}
			case provider.EventBlockStop:
				delete(open, ev.Index)
			case provider.EventBlockDelta:
				if open[ev.Index] == provider.BlockText {
					b.WriteString(ev.Delta)
				}
			}
		}
	}
}
