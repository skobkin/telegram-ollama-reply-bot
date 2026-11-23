package app

import (
	"log/slog"
	"time"

	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/imagegen"
	"telegram-ollama-reply-bot/internal/tooluse"
)

// imageGenerationTurnReserve is the time an image turn needs beyond the
// generation itself: the model call that picks the tool and the one that
// writes the reply afterwards. It sizes the boot-time deadline warning; the
// tool enforces its own, tighter bounds per call.
const imageGenerationTurnReserve = 30 * time.Second

// attachImageGeneration registers generate_image on the runtime only when a
// generation service exists. The nil check happens on the concrete pointer on
// purpose: passing a nil *imagegen.Service as the ImageGenerator interface
// would produce a non-nil interface value, and the runtime would register a
// tool whose every call dereferences nil.
func attachImageGeneration(tools *tooluse.Runtime, svc *imagegen.Service, chatCooldown time.Duration) {
	if svc == nil {
		return
	}

	tools.AttachImageGenerator(svc, chatCooldown)
}

// logImageGenerationConfig reports whether image generation is configured and
// warns when the turn deadline cannot fit a generation. Only non-sensitive
// settings are logged: the endpoint URL is never included.
func logImageGenerationConfig(logger *slog.Logger, cfg *config.Config) {
	ig := cfg.ImageGeneration
	if !ig.Enabled() {
		logger.Info("image generation disabled")

		return
	}

	logger.Info(
		"image generation configured",
		"backend", ig.Backend,
		"timeout", ig.Timeout,
		"max_image_bytes", ig.MaxImageBytes,
		"max_concurrent", ig.MaxConcurrent,
		"chat_cooldown", ig.ChatCooldown,
	)

	if warning, short := imageGenerationDeadlineShortfall(cfg); short {
		logger.Warn(
			"bot processing timeout is too short for image generation: the tool refuses to start a request that cannot finish in the turn, so image requests may be declined; raise BOT__PROCESSING_TIMEOUT",
			"processing_timeout", cfg.Bot.ProcessingTimeout,
			"recommended_minimum", warning,
		)
	}
}

// imageGenerationDeadlineShortfall reports whether the turn deadline
// (BOT__PROCESSING_TIMEOUT) is shorter than a full generation plus the model
// calls around it, and the recommended minimum. A non-positive processing
// timeout means turns have no deadline, so there is nothing to warn about.
func imageGenerationDeadlineShortfall(cfg *config.Config) (time.Duration, bool) {
	if !cfg.ImageGeneration.Enabled() || cfg.Bot.ProcessingTimeout <= 0 {
		return 0, false
	}

	recommended := cfg.ImageGeneration.Timeout + imageGenerationTurnReserve

	return recommended, cfg.Bot.ProcessingTimeout < recommended
}
