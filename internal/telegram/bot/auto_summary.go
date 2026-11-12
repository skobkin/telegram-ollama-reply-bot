package bot

import (
	"context"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"telegram-ollama-reply-bot/internal/adminconfig"
	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/tooluse"

	"github.com/getsentry/sentry-go"
	t "github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"
)

// llmSummarizer decouples the bot from the concrete llm.Service so tests can
// fake summarization (mirrors imageDescriber). *llm.Service satisfies it.
type llmSummarizer interface {
	Summarize(ctx context.Context, scope llm.PromptScope, text string, instructions string) (string, *llm.TokenUsage, error)
}

// autoSummaryMaxTrackedURLsPerChat bounds the per-chat cooldown map: once the
// limit is reached, the oldest entries are evicted. It is an internal safety
// bound, not operator configuration.
const autoSummaryMaxTrackedURLsPerChat = 64

// autoSummaryTracker is the ephemeral per-chat state of the auto-summarizer:
// the URL cooldown (a summarized URL is not re-summarized within the
// cooldown window) and the per-chat single-flight guard. Everything is
// in-memory and resets on restart by design — it must never become durable
// chat state.
type autoSummaryTracker struct {
	mu         sync.Mutex
	seen       map[int64]map[string]time.Time
	busy       map[int64]struct{}
	maxPerChat int
}

func newAutoSummaryTracker(maxPerChat int) *autoSummaryTracker {
	if maxPerChat <= 0 {
		maxPerChat = autoSummaryMaxTrackedURLsPerChat
	}

	return &autoSummaryTracker{
		seen:       make(map[int64]map[string]time.Time),
		busy:       make(map[int64]struct{}),
		maxPerChat: maxPerChat,
	}
}

// reserve claims a URL for processing. It returns false when the URL was
// summarized (or deliberately skipped) within the cooldown window — the
// entry stays — or claims it by recording the current time when fresh.
// Expired entries are pruned on the way.
func (t *autoSummaryTracker) reserve(chatID int64, url string, now time.Time, cooldown time.Duration) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	urls := t.seen[chatID]
	for seen, at := range urls {
		if now.Sub(at) >= cooldown {
			delete(urls, seen)
		}
	}
	if at, ok := urls[url]; ok && now.Sub(at) < cooldown {
		return false
	}
	if urls == nil {
		urls = make(map[string]time.Time)
		t.seen[chatID] = urls
	}
	for len(urls) >= t.maxPerChat {
		oldestURL := ""
		var oldestAt time.Time
		for seen, at := range urls {
			if oldestURL == "" || at.Before(oldestAt) {
				oldestURL, oldestAt = seen, at
			}
		}
		delete(urls, oldestURL)
	}
	urls[url] = now

	return true
}

// release drops a reservation after a failure so a repost can retry.
func (t *autoSummaryTracker) release(chatID int64, url string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if urls := t.seen[chatID]; urls != nil {
		delete(urls, url)
	}
}

// beginChat claims the per-chat single-flight slot. It returns false while
// another auto-summary job is running for the chat.
func (t *autoSummaryTracker) beginChat(chatID int64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	if _, running := t.busy[chatID]; running {
		return false
	}
	t.busy[chatID] = struct{}{}

	return true
}

func (t *autoSummaryTracker) endChat(chatID int64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	delete(t.busy, chatID)
}

// autoSummaryTargetURLs is the pure decision function of the mode: it
// returns the distinct http(s) URLs of a message that qualify for automatic
// summarization, capped at maxLinks. Messages from other bots, command
// messages, and conversational triggers (messages the bot already answers)
// never qualify. Entity-only links are covered because extractMessageText
// merges them into the message text.
func autoSummaryTargetURLs(message t.Message, enabled, conversationalTrigger bool, maxLinks int) []string {
	if !enabled || conversationalTrigger || maxLinks <= 0 {
		return nil
	}
	if message.From != nil && message.From.IsBot {
		return nil
	}

	text := strings.TrimSpace(extractMessageText(message))
	if text == "" || strings.HasPrefix(text, "/") {
		return nil
	}

	candidates := tooluse.ExtractHTTPURLs(text)
	if len(candidates) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(candidates))
	result := make([]string, 0, min(len(candidates), maxLinks))
	for _, candidate := range candidates {
		if _, dup := seen[candidate]; dup {
			continue
		}
		seen[candidate] = struct{}{}
		if len(result) >= maxLinks {
			break
		}
		result = append(result, candidate)
	}

	return result
}

// maybeAutoSummarize is the auto-summarizer entry point of the message
// pipeline. It reserves matching URLs synchronously (so concurrent delivery
// of the same link cannot start two fetches) and hands the actual work to a
// detached goroutine: ordinary messages are high-frequency, and fetching
// must never delay the update loop. The handler context dies with the
// handler, so the job derives its contexts from the bot root instead.
func (b *Bot) maybeAutoSummarize(handlerCtx *th.Context, message t.Message, resolved adminconfig.ResolvedChatConfig) {
	urls := autoSummaryTargetURLs(message, resolved.AutoSummaryEnabled, false, b.cfg.AutoSummary.MaxLinksPerMessage)
	if len(urls) == 0 {
		return
	}

	logger := b.handlerLogger(handlerCtx)
	chatID := message.Chat.ID
	now := time.Now()

	pending := make([]string, 0, len(urls))
	for _, url := range urls {
		if reserved := b.autoSummaryURLs.reserve(chatID, url, now, b.cfg.AutoSummary.URLCooldown); !reserved {
			logger.Debug("auto-summary url on cooldown", "chat_id", chatID, "url_host", autoSummaryURLHost(url))

			continue
		}
		pending = append(pending, url)
	}
	if len(pending) == 0 {
		return
	}

	if !b.autoSummaryURLs.beginChat(chatID) {
		for _, url := range pending {
			b.autoSummaryURLs.release(chatID, url)
		}
		logger.Debug("auto-summary already running for chat", "chat_id", chatID)

		return
	}

	logger.Info("auto-summarizing linked content", "chat_id", chatID, "topic_id", message.MessageThreadID, "links", len(pending))

	go func() {
		defer b.autoSummaryURLs.endChat(chatID)
		b.runAutoSummaryJob(logger, message, pending)
	}()
}

// runAutoSummaryJob processes the reserved URLs of one message sequentially.
// A link whose processing failed (or was cut short by shutdown) releases its
// cooldown reservation so a repost can retry; a deliberately skipped or
// successfully summarized link keeps it.
func (b *Bot) runAutoSummaryJob(logger *slog.Logger, message t.Message, urls []string) {
	chatID := message.Chat.ID

	for _, url := range urls {
		if shutdown := b.ctx.Err() != nil; shutdown {
			b.autoSummaryURLs.release(chatID, url)

			continue
		}
		if processed := b.autoSummarizeLink(b.ctx, logger, message, url); processed {
			continue
		}
		b.autoSummaryURLs.release(chatID, url)
	}
}

// autoSummarizeLink fetches one URL, summarizes it when the content is long
// enough, and posts the summary as a reply to the triggering message. It
// reports whether the link was fully processed (summary posted or content
// deliberately skipped); any failure returns false. Failures are silent by
// design: the summary is unprompted, so error replies would be spam.
func (b *Bot) autoSummarizeLink(baseCtx context.Context, logger *slog.Logger, message t.Message, url string) bool {
	chatID := message.Chat.ID
	processed := false

	err := b.runWithTimeout(baseCtx, tu.ID(chatID), func(ctx context.Context) error {
		article, err := b.extractor.GetArticleFromURL(ctx, url)
		if err != nil {
			logger.Warn("auto-summary extraction failed", "chat_id", chatID, "error", err, "url_host", autoSummaryURLHost(url))
			sentry.CaptureException(err)

			return nil
		}

		// Markdown is the preferred content: it preserves the article
		// structure (headings, lists, links, tables) for the summarization
		// model. Plain text is the fallback, mirroring /summarize.
		content := article.Markdown
		if content == "" {
			content = article.Text
		}
		contentLength := utf8.RuneCountInString(strings.TrimSpace(content))

		if contentLength < b.cfg.AutoSummary.MinTextLength {
			logger.Info("auto-summary skipped: content too short",
				"chat_id", chatID, "url_host", autoSummaryURLHost(url), "text_length", contentLength)
			processed = true

			return nil
		}

		summary, _, err := b.llm.Summarize(ctx, llm.PromptScope{ChatID: chatID}, content, "")
		if err != nil {
			logger.Warn("auto-summary llm request failed", "chat_id", chatID, "error", err, "url_host", autoSummaryURLHost(url))
			sentry.CaptureException(err)

			return nil
		}
		summary = strings.TrimSpace(summary)
		if summary == "" {
			logger.Warn("auto-summary produced empty reply", "chat_id", chatID, "url_host", autoSummaryURLHost(url))

			return nil
		}

		footer := "\n\n[src](" + b.sanitizer.EscapeURL(article.URL) + ")"
		sent, err := b.sendRichMarkdownReplyWithSuffix(ctx, message, summary, footer)
		if err != nil {
			logger.Warn("auto-summary send failed", "chat_id", chatID, "error", err, "url_host", autoSummaryURLHost(url))
			sentry.CaptureException(err)

			return nil
		}
		replyMessageID := 0
		if sent != nil {
			replyMessageID = sent.MessageID
		}
		b.saveBotReplyToHistory(ctx, message, summary+footer, replyMessageID)
		logger.Info("auto-summary posted",
			"chat_id", chatID, "url_host", autoSummaryURLHost(url),
			"text_length", contentLength, "summary_length", len([]rune(summary)))
		processed = true

		return nil
	})
	if err != nil {
		logger.Warn("auto-summary link processing failed", "chat_id", chatID, "error", err, "url_host", autoSummaryURLHost(url))
	}

	return processed
}

func autoSummaryURLHost(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}

	return parsed.Hostname()
}
