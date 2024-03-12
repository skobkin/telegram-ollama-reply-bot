package llm

type RequestContext struct {
	Empty  bool
	Inline bool
	User   UserContext
	Chat   ChatContext
}

type UserContext struct {
	Username  string
	FirstName string
	LastName  string
	IsPremium bool
}

type ChatContext struct {
	Title       string
	Description string
	Type        string
}

func (c RequestContext) Prompt() string {
	if c.Empty {
		return ""
	}

	prompt := ""
	if !c.Inline {
		prompt += "The type of chat you're in is \"" + c.Chat.Type + "\". "

		if c.Chat.Title != "" {
			prompt += "Chat is called \"" + c.Chat.Title + "\". "
		}
		if c.Chat.Description != "" {
			prompt += "Chat description is \"" + c.Chat.Description + "\". "
		}
	} else {
		prompt += "You're responding to inline query, so you're not in the chat right now. "
	}

	prompt += "According to their profile, first name of the user who wrote you is \"" + c.User.FirstName + "\". "
	if c.User.Username != "" {
		prompt += "Their username is @" + c.User.Username + ". "
	}
	if c.User.LastName != "" {
		prompt += "Their last name is \"" + c.User.LastName + "\". "
	}
	if c.User.IsPremium {
		prompt += "They have Telegram Premium subscription. "
	}

	return prompt
}
