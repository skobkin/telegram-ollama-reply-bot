package bot

import (
	"strings"

	t "github.com/mymmrac/telego"
)

func extractMessageText(message t.Message) string {
	body := strings.TrimSpace(message.Text)
	entities := message.Entities
	if body == "" {
		body = strings.TrimSpace(message.Caption)
		entities = message.CaptionEntities
	}

	links := appendEntityLinks(nil, entities)
	if len(links) == 0 {
		return body
	}
	if body == "" {
		return strings.Join(links, "\n")
	}

	var sb strings.Builder
	sb.WriteString(body)
	for _, link := range links {
		if strings.Contains(body, link) {
			continue
		}
		sb.WriteString("\n")
		sb.WriteString(link)
	}

	return sb.String()
}

func appendEntityLinks(dst []string, entities []t.MessageEntity) []string {
	if len(entities) == 0 {
		return dst
	}

	seen := make(map[string]struct{}, len(dst))
	for _, existing := range dst {
		seen[existing] = struct{}{}
	}

	for _, entity := range entities {
		if entity.Type != t.EntityTypeTextLink {
			continue
		}
		link := strings.TrimSpace(entity.URL)
		if link == "" {
			continue
		}
		if _, ok := seen[link]; ok {
			continue
		}
		seen[link] = struct{}{}
		dst = append(dst, link)
	}

	return dst
}
