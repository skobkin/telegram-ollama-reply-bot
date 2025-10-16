package bot

import "sync"

type ImageCache struct {
	mu    sync.RWMutex
	items map[string]string
}

func NewImageCache() *ImageCache {
	return &ImageCache{items: make(map[string]string)}
}

func (c *ImageCache) Get(imageMeta *ImageMeta) (string, bool) {
	if imageMeta == nil {
		return "", false
	}

	key := imageMeta.cacheKey()
	if key == "" {
		return "", false
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	desc, ok := c.items[key]
	return desc, ok
}

func (c *ImageCache) Set(imageMeta *ImageMeta, description string) {
	if imageMeta == nil {
		return
	}

	key := imageMeta.cacheKey()
	if key == "" {
		return
	}

	c.mu.Lock()
	c.items[key] = description
	c.mu.Unlock()
}
