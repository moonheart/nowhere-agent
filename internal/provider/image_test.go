package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type fakeResolver struct {
	byPath map[string][]byte
}

func (f fakeResolver) ResolveImage(_ context.Context, path string) ([]byte, error) {
	if b, ok := f.byPath[path]; ok {
		return b, nil
	}
	return nil, errors.New("not found")
}

func TestMaterializeImagesFillsBase64(t *testing.T) {
	res := fakeResolver{byPath: map[string][]byte{"img/a.webp": {1, 2, 3}}}
	req := Request{Messages: []Message{
		{Role: RoleUser, Content: []Block{
			{Type: BlockImage, MediaType: "image/webp", ImagePath: "img/a.webp"},
		}},
	}}
	out := MaterializeImages(context.Background(), req, res)
	b := out.Messages[0].Content[0]
	want := base64.StdEncoding.EncodeToString([]byte{1, 2, 3})
	if b.ImageData != want {
		t.Errorf("ImageData = %q, want %q", b.ImageData, want)
	}
	if b.MediaType != "image/webp" || b.ImagePath != "img/a.webp" {
		t.Errorf("block metadata lost: %+v", b)
	}
}

func TestMaterializeImagesByteStable(t *testing.T) {
	res := fakeResolver{byPath: map[string][]byte{"img/a.webp": {9, 8, 7}}}
	mk := func() Request {
		return Request{Messages: []Message{{Role: RoleUser, Content: []Block{
			{Type: BlockImage, MediaType: "image/webp", ImagePath: "img/a.webp"},
		}}}}
	}
	a := MaterializeImages(context.Background(), mk(), res)
	b := MaterializeImages(context.Background(), mk(), res)
	if a.Messages[0].Content[0].ImageData != b.Messages[0].Content[0].ImageData {
		t.Error("materialization not byte-stable across calls (prompt cache would bust)")
	}
}

func TestMaterializeImagesDanglingLeavesPlaceholder(t *testing.T) {
	res := fakeResolver{byPath: map[string][]byte{}}
	req := Request{Messages: []Message{{Role: RoleUser, Content: []Block{
		{Type: BlockImage, MediaType: "image/webp", ImagePath: "img/gone.webp"},
	}}}}
	out := MaterializeImages(context.Background(), req, res)
	if out.Messages[0].Content[0].ImageData != "" {
		t.Error("dangling image should leave ImageData empty (→ text placeholder)")
	}
}

func TestMaterializeImagesSkipsNonImageAndPrefilled(t *testing.T) {
	res := fakeResolver{byPath: map[string][]byte{"img/a.webp": {1}}}
	req := Request{Messages: []Message{{Role: RoleUser, Content: []Block{
		{Type: BlockText, Text: "hi"},
		{Type: BlockImage, MediaType: "image/webp", ImagePath: "img/a.webp", ImageData: "already"},
	}}}}
	out := MaterializeImages(context.Background(), req, res)
	if out.Messages[0].Content[1].ImageData != "already" {
		t.Error("pre-filled ImageData must not be overwritten")
	}
}

func TestMaterializeImagesNilResolver(t *testing.T) {
	req := Request{Messages: []Message{{Role: RoleUser, Content: []Block{
		{Type: BlockImage, MediaType: "image/webp", ImagePath: "img/a.webp"},
	}}}}
	out := MaterializeImages(context.Background(), req, nil)
	if out.Messages[0].Content[0].ImageData != "" {
		t.Error("nil resolver must leave blocks untouched")
	}
}

// TestImageDataNeverSerialized guards the core invariant (persist-raw-messages
// D6): the materialized base64 payload must never enter the persisted form.
// Only the path pointer and media type persist.
func TestImageDataNeverSerialized(t *testing.T) {
	b := Block{Type: BlockImage, MediaType: "image/webp", ImagePath: "img/a.webp", ImageData: "QUJDREVG"}
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if strings.Contains(s, "QUJDREVG") || strings.Contains(s, "ImageData") || strings.Contains(s, "image_data") {
		t.Errorf("base64 payload leaked into serialized block: %s", s)
	}
	if !strings.Contains(s, "img/a.webp") || !strings.Contains(s, "image/webp") {
		t.Errorf("pointer/media_type must persist: %s", s)
	}
}
