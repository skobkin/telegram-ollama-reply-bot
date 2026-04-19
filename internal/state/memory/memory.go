package memory

import (
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"telegram-ollama-reply-bot/internal/state"

	"github.com/getsentry/sentry-go"
)

const (
	defaultMaxBytes                 int64         = 256 << 20 // 256 MiB
	defaultHistoryMaxBytes          int64         = 160 << 20 // 160 MiB
	defaultHistoryStreamsMax                      = 1024
	defaultHistoryMessagesPerStream               = 150
	defaultImageCacheMaxBytes       int64         = 64 << 20 // 64 MiB
	defaultImageCacheTTL            time.Duration = 24 * time.Hour
)

type Config struct {
	MaxBytes                 int64
	HistoryMaxBytes          int64
	HistoryStreamsMax        int
	HistoryMessagesPerStream int
	ImageCacheMaxBytes       int64
	ImageCacheTTL            time.Duration
}

func (c Config) withDefaults() Config {
	if c.MaxBytes <= 0 {
		c.MaxBytes = defaultMaxBytes
	}
	if c.HistoryMaxBytes <= 0 {
		c.HistoryMaxBytes = defaultHistoryMaxBytes
	}
	if c.HistoryStreamsMax <= 0 {
		c.HistoryStreamsMax = defaultHistoryStreamsMax
	}
	if c.HistoryMessagesPerStream <= 0 {
		c.HistoryMessagesPerStream = defaultHistoryMessagesPerStream
	}
	if c.ImageCacheMaxBytes <= 0 {
		c.ImageCacheMaxBytes = defaultImageCacheMaxBytes
	}
	if c.ImageCacheTTL <= 0 {
		c.ImageCacheTTL = defaultImageCacheTTL
	}

	return c
}

type Stores struct {
	conversations *ConversationStore
	images        *ImageStore
	stats         *StatsStore
}

func New(cfg Config, logger *slog.Logger) *Stores {
	cfg = cfg.withDefaults()
	if logger == nil {
		logger = slog.Default()
	}
	budget := newBudgetTracker(cfg.MaxBytes, logger)

	return &Stores{
		conversations: newConversationStore(cfg, logger.With("bucket", "conversation"), budget),
		images:        newImageStore(cfg, logger.With("bucket", "image_cache"), budget),
		stats:         newStatsStore(),
	}
}

func (s *Stores) Conversations() state.ConversationStore {
	return s.conversations
}

func (s *Stores) Images() state.ImageStore {
	return s.images
}

func (s *Stores) Stats() state.StatsStore {
	return s.stats
}

type conversationBucket struct {
	scope           state.ConversationScope
	messages        []state.Message
	earlierSummary  string
	summarizedUntil int
	bytes           int64
	lastUpdatedAt   time.Time
}

type ConversationStore struct {
	mu sync.RWMutex

	logger           *slog.Logger
	budget           *budgetTracker
	maxBytes         int64
	maxStreams       int
	maxMessages      int
	totalBytes       int64
	streams          map[string]*conversationBucket
	streamOrder      []string
	chatIndex        map[int64]map[string]struct{}
	topicIndex       map[int64]map[string]struct{}
	userIndex        map[int64]map[string]struct{}
	featureTypeIndex map[string]map[string]struct{}
}

func newConversationStore(cfg Config, logger *slog.Logger, budget *budgetTracker) *ConversationStore {
	return &ConversationStore{
		logger:           logger,
		budget:           budget,
		maxBytes:         cfg.HistoryMaxBytes,
		maxStreams:       cfg.HistoryStreamsMax,
		maxMessages:      cfg.HistoryMessagesPerStream,
		streams:          make(map[string]*conversationBucket),
		streamOrder:      make([]string, 0, cfg.HistoryStreamsMax),
		chatIndex:        make(map[int64]map[string]struct{}),
		topicIndex:       make(map[int64]map[string]struct{}),
		userIndex:        make(map[int64]map[string]struct{}),
		featureTypeIndex: make(map[string]map[string]struct{}),
	}
}

func (s *ConversationStore) AppendMessage(scope state.ConversationScope, msg state.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := scopeKey(scope)
	bucket, ok := s.streams[key]
	if !ok {
		bucket = &conversationBucket{scope: scope}
		s.streams[key] = bucket
		s.streamOrder = append(s.streamOrder, key)
		s.addStreamIndex(scope, key)
	}

	msg.ChatID = scope.ChatID
	msg.TopicID = scope.TopicID
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now().UTC()
	}

	if len(bucket.messages) >= s.maxMessages {
		removed := bucket.messages[0]
		bucket.messages = bucket.messages[1:]
		size := messageApproxBytes(removed)
		bucket.bytes -= size
		s.totalBytes -= size
		if bucket.summarizedUntil > 0 {
			bucket.summarizedUntil--
		}
		s.dropUserIndex(key, removed)
	}

	cloned := cloneMessage(msg)
	bucket.messages = append(bucket.messages, cloned)
	size := messageApproxBytes(cloned)
	bucket.bytes += size
	bucket.lastUpdatedAt = time.Now().UTC()
	s.totalBytes += size
	s.addUserIndex(key, cloned)
	s.touchStreamOrder(key)
	s.evictIfNeeded()
	s.budget.Set("conversation", s.totalBytes)
}

func (s *ConversationStore) Snapshot(scope state.ConversationScope) state.ConversationSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	bucket, ok := s.streams[scopeKey(scope)]
	if !ok {
		return state.ConversationSnapshot{Messages: make([]state.Message, 0)}
	}

	result := state.ConversationSnapshot{
		Messages:         cloneMessages(bucket.messages),
		EarlierSummary:   bucket.earlierSummary,
		SummarizedUntil:  bucket.summarizedUntil,
		MessageCount:     len(bucket.messages),
		ApproxBytes:      bucket.bytes,
		LastUpdatedAtUTC: bucket.lastUpdatedAt,
	}

	return result
}

func (s *ConversationStore) SetEarlierSummary(scope state.ConversationScope, text string, summarizedUntil int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := scopeKey(scope)
	bucket, ok := s.streams[key]
	if !ok {
		bucket = &conversationBucket{scope: scope}
		s.streams[key] = bucket
		s.streamOrder = append(s.streamOrder, key)
		s.addStreamIndex(scope, key)
	}

	oldSize := int64(len(bucket.earlierSummary))
	newSize := int64(len(text))
	bucket.earlierSummary = text
	bucket.summarizedUntil = summarizedUntil
	bucket.lastUpdatedAt = time.Now().UTC()
	bucket.bytes += newSize - oldSize
	s.totalBytes += newSize - oldSize
	s.touchStreamOrder(key)
	s.evictIfNeeded()
	s.budget.Set("conversation", s.totalBytes)
}

func (s *ConversationStore) Reset(scope state.ConversationScope) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeStream(scopeKey(scope))
	s.budget.Set("conversation", s.totalBytes)
}

func (s *ConversationStore) ResetChat(chatID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	keys := s.chatIndex[chatID]
	for key := range keys {
		s.removeStream(key)
	}
	s.budget.Set("conversation", s.totalBytes)
}

func (s *ConversationStore) addStreamIndex(scope state.ConversationScope, key string) {
	addIndexKey(s.chatIndex, scope.ChatID, key)
	addIndexKey(s.topicIndex, topicIndexKey(scope), key)
	addStringIndexKey(s.featureTypeIndex, "conversation", key)
}

func (s *ConversationStore) addUserIndex(key string, msg state.Message) {
	if msg.FromID != 0 {
		addIndexKey(s.userIndex, msg.FromID, key)
	}
	if msg.ReplyTo != nil {
		s.addUserIndex(key, *msg.ReplyTo)
	}
}

func (s *ConversationStore) dropUserIndex(key string, msg state.Message) {
	if msg.FromID != 0 {
		dropIndexKey(s.userIndex, msg.FromID, key)
	}
	if msg.ReplyTo != nil {
		s.dropUserIndex(key, *msg.ReplyTo)
	}
}

func (s *ConversationStore) removeStream(key string) {
	bucket, ok := s.streams[key]
	if !ok {
		return
	}

	delete(s.streams, key)
	s.totalBytes -= bucket.bytes
	for _, msg := range bucket.messages {
		s.dropUserIndex(key, msg)
	}
	dropIndexKey(s.chatIndex, bucket.scope.ChatID, key)
	dropIndexKey(s.topicIndex, topicIndexKey(bucket.scope), key)
	dropStringIndexKey(s.featureTypeIndex, "conversation", key)
	s.removeFromOrder(key)
}

func (s *ConversationStore) removeFromOrder(key string) {
	for i, current := range s.streamOrder {
		if current == key {
			s.streamOrder = append(s.streamOrder[:i], s.streamOrder[i+1:]...)

			return
		}
	}
}

func (s *ConversationStore) touchStreamOrder(key string) {
	s.removeFromOrder(key)
	s.streamOrder = append(s.streamOrder, key)
}

func (s *ConversationStore) evictIfNeeded() {
	for len(s.streams) > s.maxStreams {
		s.evictOldestStream("stream_count_limit")
	}

	for s.totalBytes > s.maxBytes && len(s.streamOrder) > 0 {
		s.evictOldestStream("history_byte_limit")
	}
}

func (s *ConversationStore) evictOldestStream(reason string) {
	if len(s.streamOrder) == 0 {
		return
	}

	oldest := s.streamOrder[0]
	s.streamOrder = s.streamOrder[1:]
	bucket := s.streams[oldest]
	if bucket != nil {
		s.logger.Warn("evicting conversation stream", "reason", reason, "chat_id", bucket.scope.ChatID, "topic_id", bucket.scope.TopicID)
	}
	s.removeStream(oldest)
}

type imageEntry struct {
	key        string
	desc       string
	expiresAt  time.Time
	bytes      int64
	lastAccess time.Time
}

type ImageStore struct {
	mu sync.RWMutex

	logger           *slog.Logger
	budget           *budgetTracker
	maxBytes         int64
	ttl              time.Duration
	totalBytes       int64
	items            map[string]*imageEntry
	order            []string
	featureTypeIndex map[string]map[string]struct{}
}

func newImageStore(cfg Config, logger *slog.Logger, budget *budgetTracker) *ImageStore {
	return &ImageStore{
		logger:           logger,
		budget:           budget,
		maxBytes:         cfg.ImageCacheMaxBytes,
		ttl:              cfg.ImageCacheTTL,
		items:            make(map[string]*imageEntry),
		order:            make([]string, 0),
		featureTypeIndex: map[string]map[string]struct{}{"image_cache": {}},
	}
}

func (s *ImageStore) Get(imageMeta *state.ImageMeta) (string, bool) {
	if imageMeta == nil {
		return "", false
	}

	key := imageMeta.CacheKey()
	if key == "" {
		return "", false
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeExpiredLocked(time.Now().UTC())

	entry, ok := s.items[key]
	if !ok {
		return "", false
	}

	entry.lastAccess = time.Now().UTC()

	return entry.desc, true
}

func (s *ImageStore) Set(imageMeta *state.ImageMeta, description string) {
	if imageMeta == nil {
		return
	}

	key := imageMeta.CacheKey()
	if key == "" {
		return
	}

	now := time.Now().UTC()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.removeExpiredLocked(now)

	if existing, ok := s.items[key]; ok {
		s.totalBytes -= existing.bytes
		existing.desc = description
		existing.bytes = int64(len(key) + len(description))
		existing.expiresAt = now.Add(s.ttl)
		existing.lastAccess = now
		s.totalBytes += existing.bytes
		s.touchOrder(key)
	} else {
		entry := &imageEntry{
			key:        key,
			desc:       description,
			expiresAt:  now.Add(s.ttl),
			lastAccess: now,
			bytes:      int64(len(key) + len(description)),
		}
		s.items[key] = entry
		s.totalBytes += entry.bytes
		s.order = append(s.order, key)
	}

	addStringIndexKey(s.featureTypeIndex, "image_cache", key)
	for s.totalBytes > s.maxBytes && len(s.order) > 0 {
		s.evictOldestLocked("image_cache_byte_limit")
	}
	s.budget.Set("image_cache", s.totalBytes)
}

func (s *ImageStore) removeExpiredLocked(now time.Time) {
	for key, entry := range s.items {
		if now.After(entry.expiresAt) {
			s.totalBytes -= entry.bytes
			delete(s.items, key)
			dropStringIndexKey(s.featureTypeIndex, "image_cache", key)
			s.removeOrderKey(key)
		}
	}
	s.budget.Set("image_cache", s.totalBytes)
}

func (s *ImageStore) evictOldestLocked(reason string) {
	if len(s.order) == 0 {
		return
	}

	key := s.order[0]
	s.order = s.order[1:]
	entry, ok := s.items[key]
	if !ok {
		return
	}

	delete(s.items, key)
	dropStringIndexKey(s.featureTypeIndex, "image_cache", key)
	s.totalBytes -= entry.bytes
	s.logger.Warn("evicting image cache entry", "reason", reason, "key", key)
}

func (s *ImageStore) touchOrder(key string) {
	s.removeOrderKey(key)
	s.order = append(s.order, key)
}

func (s *ImageStore) removeOrderKey(key string) {
	for i, current := range s.order {
		if current == key {
			s.order = append(s.order[:i], s.order[i+1:]...)

			return
		}
	}
}

type StatsStore struct {
	mu sync.Mutex

	runningSince time.Time

	groupRequests   uint64
	privateRequests uint64
	inlineQueries   uint64
	mentions        uint64
	summarize       uint64
	chatResets      uint64
	promptTokens    uint64
	completion      uint64
	totalTokens     uint64
	totalCost       float64
	llmTimeouts     uint64
}

func newStatsStore() *StatsStore {
	return &StatsStore{runningSince: time.Now()}
}

func (s *StatsStore) Snapshot() state.StatsSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	return state.StatsSnapshot{
		RunningSince:      s.runningSince,
		Uptime:            time.Since(s.runningSince).String(),
		GroupRequests:     s.groupRequests,
		PrivateRequests:   s.privateRequests,
		InlineQueries:     s.inlineQueries,
		Mentions:          s.mentions,
		SummarizeRequests: s.summarize,
		ChatHistoryResets: s.chatResets,
		PromptTokens:      s.promptTokens,
		CompletionTokens:  s.completion,
		TotalTokens:       s.totalTokens,
		TotalCost:         s.totalCost,
		LlmTimeouts:       s.llmTimeouts,
	}
}

func (s *StatsStore) String() string    { return s.Snapshot().String() }
func (s *StatsStore) InlineQuery()      { s.mu.Lock(); s.inlineQueries++; s.mu.Unlock() }
func (s *StatsStore) GroupRequest()     { s.mu.Lock(); s.groupRequests++; s.mu.Unlock() }
func (s *StatsStore) PrivateRequest()   { s.mu.Lock(); s.privateRequests++; s.mu.Unlock() }
func (s *StatsStore) Mention()          { s.mu.Lock(); s.mentions++; s.mu.Unlock() }
func (s *StatsStore) SummarizeRequest() { s.mu.Lock(); s.summarize++; s.mu.Unlock() }
func (s *StatsStore) ChatHistoryReset() { s.mu.Lock(); s.chatResets++; s.mu.Unlock() }
func (s *StatsStore) LlmTimeout()       { s.mu.Lock(); s.llmTimeouts++; s.mu.Unlock() }
func (s *StatsStore) AddUsage(prompt, completion, total int, cost float64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if prompt < 0 || completion < 0 || total < 0 {
		sentry.CaptureException(errors.New("state stats: negative token usage"))

		return
	}

	s.promptTokens += uint64(prompt)
	s.completion += uint64(completion)
	s.totalTokens += uint64(total)
	s.totalCost += cost
}

func scopeKey(scope state.ConversationScope) string {
	return timeKeyPart(scope.ChatID) + ":" + timeKeyPart(int64(scope.TopicID))
}

func timeKeyPart(v int64) string {
	return strconv.FormatInt(v, 10)
}

func topicIndexKey(scope state.ConversationScope) int64 {
	return (scope.ChatID << 32) ^ int64(scope.TopicID)
}

func addIndexKey(index map[int64]map[string]struct{}, idx int64, key string) {
	if _, ok := index[idx]; !ok {
		index[idx] = make(map[string]struct{})
	}
	index[idx][key] = struct{}{}
}

func dropIndexKey(index map[int64]map[string]struct{}, idx int64, key string) {
	keys := index[idx]
	if keys == nil {
		return
	}
	delete(keys, key)
	if len(keys) == 0 {
		delete(index, idx)
	}
}

func addStringIndexKey(index map[string]map[string]struct{}, idx, key string) {
	if _, ok := index[idx]; !ok {
		index[idx] = make(map[string]struct{})
	}
	index[idx][key] = struct{}{}
}

func dropStringIndexKey(index map[string]map[string]struct{}, idx, key string) {
	keys := index[idx]
	if keys == nil {
		return
	}
	delete(keys, key)
	if len(keys) == 0 {
		delete(index, idx)
	}
}

func cloneMessages(messages []state.Message) []state.Message {
	if len(messages) == 0 {
		return make([]state.Message, 0)
	}

	result := make([]state.Message, 0, len(messages))
	for _, msg := range messages {
		result = append(result, cloneMessage(msg))
	}

	return result
}

func cloneMessage(msg state.Message) state.Message {
	cloned := msg
	if msg.ReplyTo != nil {
		reply := cloneMessage(*msg.ReplyTo)
		cloned.ReplyTo = &reply
	}
	if msg.ImageMeta != nil {
		meta := *msg.ImageMeta
		cloned.ImageMeta = &meta
	}

	return cloned
}

func messageApproxBytes(msg state.Message) int64 {
	size := int64(len(msg.Name) + len(msg.Username) + len(msg.Text) + len(msg.Image) + 64)
	if msg.ImageMeta != nil {
		size += int64(len(msg.ImageMeta.FileID) + len(msg.ImageMeta.FileUniqueID) + 32)
	}
	if msg.ReplyTo != nil {
		size += messageApproxBytes(*msg.ReplyTo)
	}

	return size
}

type budgetTracker struct {
	mu      sync.Mutex
	max     int64
	logger  *slog.Logger
	current map[string]int64
}

func newBudgetTracker(maxBytes int64, logger *slog.Logger) *budgetTracker {
	return &budgetTracker{
		max:     maxBytes,
		logger:  logger.With("bucket", "global_budget"),
		current: make(map[string]int64),
	}
}

func (b *budgetTracker) Set(name string, value int64) {
	if b == nil || b.max <= 0 {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	b.current[name] = value
	var total int64
	for _, current := range b.current {
		total += current
	}
	if total > b.max {
		b.logger.Warn("state soft limit exceeded", "max_bytes", b.max, "current_bytes", total)
	}
}
