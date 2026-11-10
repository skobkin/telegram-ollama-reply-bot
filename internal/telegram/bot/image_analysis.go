package bot

import (
	"context"
	"errors"
	"strings"

	"telegram-ollama-reply-bot/internal/state"
	"telegram-ollama-reply-bot/internal/vision"
)

// analyzeImage re-fetches a retained image and analyzes it under an explicit,
// model-authored prompt. Unlike describeImage it never reads or writes the
// baseline description cache and never mutates stored messages: the
// prompt-specific analysis is ephemeral and exists only for the current tool
// loop. Fetching, bounding, sanitization, and usage accounting are shared
// with describeImage.
func (b *Bot) analyzeImage(ctx context.Context, imageMeta *state.ImageMeta, prompt string) (vision.Recognition, error) {
	if imageMeta == nil {
		return vision.Recognition{}, ErrImageRecognition
	}

	if strings.TrimSpace(prompt) == "" {
		return vision.Recognition{}, errors.Join(ErrImageRecognition, vision.ErrPromptEmpty)
	}

	if ctx == nil {
		ctx = b.ctx
	}

	fileBytes, err := b.fetchImageFile(ctx, imageMeta)
	if err != nil {
		return vision.Recognition{}, err
	}

	recognition, err := b.vision.Analyze(ctx, fileBytes, prompt)
	if err != nil {
		return vision.Recognition{}, errors.Join(ErrImageRecognition, err)
	}

	b.accountRecognitionUsage(recognition)

	b.loggerFromContext(ctx).Debug(
		"image analyzed",
		"file_id", imageMeta.FileID,
		"image_bytes", len(fileBytes),
		"prompt_length", len(prompt),
		"analysis_length", len(recognition.Text),
	)

	return recognition, nil
}

// AnalyzeImage implements tooluse.ImageAnalyzer: it re-analyzes a retained
// image referenced by its Telegram file metadata under a model-authored
// prompt and returns only the analysis text (usage is accounted internally,
// exactly like the baseline path). The enabled guard is defense-in-depth —
// the application layer only attaches the analyzer while image recognition
// is enabled.
func (b *Bot) AnalyzeImage(ctx context.Context, meta state.ImageMeta, prompt string) (string, error) {
	if !b.imageRecognition.Enabled {
		return "", errors.Join(ErrImageRecognition, errors.New("image recognition is disabled"))
	}

	recognition, err := b.analyzeImage(ctx, &meta, prompt)
	if err != nil {
		return "", err
	}

	return recognition.Text, nil
}
