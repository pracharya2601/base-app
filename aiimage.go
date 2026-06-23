package main

import (
	"net/http"
	"strings"
	"time"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/filesystem"

	"github.com/zendev-sh/goai"
	"github.com/zendev-sh/goai/provider"
	"github.com/zendev-sh/goai/provider/azure"
	"github.com/zendev-sh/goai/provider/google"
	"github.com/zendev-sh/goai/provider/openai"
	"github.com/zendev-sh/goai/provider/vertex"
)

// Image generation (a separate goai path from text): POST /api/ai/{provider}/image
//   - builds an ImageModel (xxx.Image, NOT xxx.Chat) and calls goai.GenerateImage
//   - stores each returned image in the _aiImages collection's FILE field, so it
//     lands wherever PocketBase file storage points — local in dev, S3/R2 in prod
//     (enable Settings -> Files storage -> S3). No AWS SDK needed.
//   - returns a preview URL per image (PocketBase's /api/files/... link)
// Only a few providers support images; they reuse the same _aiProviders key.

const aiImageCollection = "_aiImages"

type aiImageProvider struct {
	desc  string
	build func(model, apiKey, baseURL string) provider.ImageModel
}

// buildImageKeyBase / buildImageKeyOnly mirror ai.go's text builders but return
// an ImageModel. Generics infer the per-package Option type.
func buildImageKeyBase[O any](
	img func(string, ...O) provider.ImageModel,
	withKey func(string) O,
	withBase func(string) O,
) func(model, apiKey, baseURL string) provider.ImageModel {
	return func(model, apiKey, baseURL string) provider.ImageModel {
		opts := []O{withKey(apiKey)}
		if baseURL != "" {
			opts = append(opts, withBase(baseURL))
		}
		return img(model, opts...)
	}
}

func buildImageKeyOnly[O any](
	img func(string, ...O) provider.ImageModel,
	withKey func(string) O,
) func(model, apiKey, baseURL string) provider.ImageModel {
	return func(model, apiKey, _ string) provider.ImageModel {
		return img(model, withKey(apiKey))
	}
}

// knownImageProviders is the image allowlist — only goai providers exposing an
// Image() constructor. Keys come from the SAME _aiProviders rows as text.
var knownImageProviders = map[string]aiImageProvider{
	"openai": {"OpenAI images (gpt-image-1, dall-e-3).", buildImageKeyBase(openai.Image, openai.WithAPIKey, openai.WithBaseURL)},
	"google": {"Google Imagen.", buildImageKeyBase(google.Image, google.WithAPIKey, google.WithBaseURL)},
	"vertex": {"Vertex AI Imagen.", buildImageKeyBase(vertex.Image, vertex.WithAPIKey, vertex.WithBaseURL)},
	"azure":  {"Azure OpenAI images.", buildImageKeyOnly(azure.Image, azure.WithAPIKey)},
}

func imageExt(mediaType string) string {
	switch mediaType {
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	default:
		return ".png"
	}
}

// ensureAIImagesCollection creates the _aiImages store. System=true keeps it out
// of RBAC backfill; VIEW is public ("") so preview URLs (capability links with a
// random filename) embed directly, while LIST stays superuser-only (nil) so the
// gallery can't be enumerated.
func ensureAIImagesCollection(app core.App) error {
	if _, err := app.FindCollectionByNameOrId(aiImageCollection); err == nil {
		return nil
	}
	col := core.NewBaseCollection(aiImageCollection)
	col.System = true
	col.Fields.Add(&core.FileField{
		Name:      "file",
		MaxSelect: 1,
		MaxSize:   20 << 20, // 20 MB
		MimeTypes: []string{"image/png", "image/jpeg", "image/webp", "image/gif"},
	})
	col.Fields.Add(&core.TextField{Name: "userId"})
	col.Fields.Add(&core.TextField{Name: "provider"})
	col.Fields.Add(&core.TextField{Name: "model"})
	col.Fields.Add(&core.TextField{Name: "prompt"})
	col.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
	public := ""
	col.ViewRule = &public // public view -> embeddable preview URLs
	return app.Save(col)
}

// baseURL prefers the configured AppURL (correct behind a proxy/custom domain),
// falling back to the request's scheme+host for local/dev.
func previewBaseURL(app core.App, e *core.RequestEvent) string {
	if u := strings.TrimRight(app.Settings().Meta.AppURL, "/"); u != "" {
		return u
	}
	scheme := "http"
	if e.Request.TLS != nil || strings.EqualFold(e.Request.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + e.Request.Host
}

func registerAIImageRoutes(se *core.ServeEvent, app core.App) {
	// Catalog of image-capable providers (for UI pickers).
	se.Router.GET("/api/ai/image-catalog", func(e *core.RequestEvent) error {
		out := make([]map[string]any, 0, len(knownImageProviders))
		for name, p := range knownImageProviders {
			out = append(out, map[string]any{"provider": name, "description": p.desc})
		}
		return e.JSON(http.StatusOK, map[string]any{"providers": out})
	}).Bind(apis.RequireAuth())

	// Generate image(s), store each to the file field, return preview URLs.
	se.Router.POST("/api/ai/{provider}/image", func(e *core.RequestEvent) error {
		name := e.Request.PathValue("provider")
		ip, ok := knownImageProviders[name]
		if !ok {
			return e.BadRequestError("provider does not support image generation: "+name, nil)
		}
		cfg, err := loadAIProvider(app, name) // reuse the same _aiProviders key
		if err != nil {
			return e.BadRequestError(err.Error(), nil)
		}

		var req struct {
			Model       string `json:"model"`
			Prompt      string `json:"prompt"`
			Size        string `json:"size"`        // e.g. "1024x1024"
			AspectRatio string `json:"aspectRatio"` // e.g. "16:9"
			Count       int    `json:"count"`       // default 1
		}
		if err := e.BindBody(&req); err != nil {
			return e.BadRequestError("invalid request body", err)
		}
		if strings.TrimSpace(req.Prompt) == "" {
			return e.BadRequestError("'prompt' is required", nil)
		}
		model := req.Model
		if model == "" {
			model = cfg.defaultModel
		}
		if model == "" {
			return e.BadRequestError("'model' is required (no defaultModel configured for this provider)", nil)
		}

		opts := []goai.ImageOption{goai.WithImagePrompt(req.Prompt)}
		if req.Size != "" {
			opts = append(opts, goai.WithImageSize(req.Size))
		}
		if req.AspectRatio != "" {
			opts = append(opts, goai.WithAspectRatio(req.AspectRatio))
		}
		if req.Count > 0 {
			opts = append(opts, goai.WithImageCount(req.Count))
		}

		im := ip.build(model, cfg.apiKey, cfg.baseURL)
		start := time.Now()
		result, gerr := goai.GenerateImage(e.Request.Context(), im, opts...)
		latency := time.Since(start).Milliseconds()
		if gerr != nil {
			logAIUsage(app, name, model, e.Auth, 0, 0, latency, "error", gerr.Error())
			return e.JSON(http.StatusBadGateway, map[string]any{"error": "provider error", "detail": gerr.Error()})
		}

		col, cerr := app.FindCollectionByNameOrId(aiImageCollection)
		if cerr != nil {
			return e.InternalServerError("images collection missing", cerr)
		}
		base := previewBaseURL(app, e)
		images := make([]map[string]any, 0, len(result.Images))
		for i, img := range result.Images {
			rec := core.NewRecord(col)
			if e.Auth != nil {
				rec.Set("userId", e.Auth.Id)
			}
			rec.Set("provider", name)
			rec.Set("model", model)
			rec.Set("prompt", req.Prompt)
			f, ferr := filesystem.NewFileFromBytes(img.Data, "image"+imageExt(img.MediaType))
			if ferr != nil {
				return e.InternalServerError("failed to wrap generated image", ferr)
			}
			rec.Set("file", f)
			if serr := app.Save(rec); serr != nil {
				return e.InternalServerError("failed to store generated image", serr)
			}
			images = append(images, map[string]any{
				"id":        rec.Id,
				"url":       base + "/api/files/" + aiImageCollection + "/" + rec.Id + "/" + rec.GetString("file"),
				"mediaType": img.MediaType,
				"index":     i,
			})
		}

		logAIUsage(app, name, model, e.Auth, result.Usage.InputTokens, result.Usage.OutputTokens, latency, "ok", "")
		return e.JSON(http.StatusOK, map[string]any{"model": model, "count": len(images), "images": images})
	}).Bind(apis.RequireAuth())
}
