package bot

import (
	"context"

	t "github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
)

// AnyMessageWithPhoto returns a predicate that matches any message with a photo
func AnyMessageWithPhoto() th.Predicate {
	return func(ctx context.Context, update t.Update) bool {
		if update.Message == nil {
			return false
		}

		return len(update.Message.Photo) > 0
	}
}
