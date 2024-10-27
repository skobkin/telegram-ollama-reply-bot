package llm

type RequestContext struct {
	Empty bool
	User  UserContext
	Chat  ChatContext
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
	History     []ChatMessage
}

type ChatMessage struct {
	Name string
	Text string
	IsMe bool
}

func (c RequestContext) Prompt() string {
	if c.Empty {
		return ""
	}

	prompt := ""

	prompt += "The type of chat you're in is \"" + c.Chat.Type + "\". "

	if c.Chat.Type == "group" || c.Chat.Type == "supergroup" {
		prompt += "Please consider that there are several users in this chat type who may discuss several unrelated " +
			"topics. Try to respond only about the topic you were asked about and only to the user who asked you, " +
			"but keep in mind another chat history. "
	}

	if c.Chat.Title != "" {
		prompt += "\nChat is called \"" + c.Chat.Title + "\". "
	}
	if c.Chat.Description != "" {
		prompt += "Chat description is \"" + c.Chat.Description + "\". "
	}

	prompt += "\nProfile of the user who mentioned you in the chat:" +
		"First name: \"" + c.User.FirstName + "\"\n"
	if c.User.Username != "" {
		prompt += "Username: @" + c.User.Username + ".\n"
	}
	if c.User.LastName != "" {
		prompt += "Last name: \"" + c.User.LastName + "\"\n"
	}
	//if c.User.IsPremium {
	//	prompt += "Telegram Premium subscription: active."
	//}

	return prompt
}
