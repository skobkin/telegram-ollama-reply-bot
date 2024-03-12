package llm

type RequestContext struct {
	User UserContext
	Chat ChatContext
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
	prompt := "The type of chat you're in is \"" + c.Chat.Type + "\". "

	if c.Chat.Title != "" {
		prompt += "Chat is called \"" + c.Chat.Title + "\". "
	}
	if c.Chat.Description != "" {
		prompt += "Chat description is \"" + c.Chat.Description + "\". "
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
