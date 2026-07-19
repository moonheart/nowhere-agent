package provider

import (
	"context"
	"encoding/base64"
)

// ImageResolver loads an image payload by workspace-relative path. It is the
// seam the materialization transform uses to read image bytes; the workspace
// layer provides the confined, session-scoped implementation. Returning an
// error marks the image unavailable (the block degrades to a placeholder).
type ImageResolver interface {
	// ResolveImage returns the raw (already WebP) image bytes for a path,
	// or an error if the path is missing/invalid/escapes the workspace.
	ResolveImage(ctx context.Context, path string) ([]byte, error)
}

// MaterializeImages rewrites every BlockImage in the request's messages so the
// provider adapter receives the image payload. For each image block it resolves
// the path to bytes and fills ImageData (base64), leaving MediaType set so the
// adapter can build the provider-native image source. Blocks whose path cannot
// be resolved are left with empty ImageData, which the adapter renders as a
// text placeholder (fail-closed, the request still sends).
//
// This runs before every send, every turn: prompt caching keys on a
// byte-identical prefix, so an image must materialize to the same base64 on
// every request. Determinism comes from the workspace file being immutable
// once written.
func MaterializeImages(ctx context.Context, req Request, res ImageResolver) Request {
	if res == nil {
		return req
	}
	for mi := range req.Messages {
		for bi := range req.Messages[mi].Content {
			b := &req.Messages[mi].Content[bi]
			if b.Type != BlockImage || b.ImageData != "" {
				continue
			}
			data, err := res.ResolveImage(ctx, b.ImagePath)
			if err != nil {
				// Leave ImageData empty → adapter emits a text placeholder.
				continue
			}
			b.ImageData = base64.StdEncoding.EncodeToString(data)
		}
	}
	return req
}
