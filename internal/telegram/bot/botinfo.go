package bot

import t "github.com/mymmrac/telego"

type botInfo struct {
	ID                      int64
	Username                string
	FirstName               string
	LastName                string
	IsBot                   bool
	CanJoinGroups           bool
	CanReadAllGroupMessages bool
	SupportsInlineQueries   bool
}

func botInfoFromUser(user *t.User) botInfo {
	if user == nil {
		return botInfo{}
	}

	return botInfo{
		ID:                      user.ID,
		Username:                user.Username,
		FirstName:               user.FirstName,
		LastName:                user.LastName,
		IsBot:                   user.IsBot,
		CanJoinGroups:           user.CanJoinGroups,
		CanReadAllGroupMessages: user.CanReadAllGroupMessages,
		SupportsInlineQueries:   user.SupportsInlineQueries,
	}
}
