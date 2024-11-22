# Telegram Ollama Bot

[![Build Status](https://ci.skobk.in/api/badges/skobkin/telegram-ollama-reply-bot/status.svg)](https://ci.skobk.in/skobkin/telegram-ollama-reply-bot)

![Project Banner](/img/banner.jpeg)

## Functionality

- Context-dependent dialogue in chats
- Summarization of articles by provided link

## Usage

### Docker

```shell
docker run \
  -e OPENAI_API_TOKEN=123 \
  -e OPENAI_API_BASE_URL=http://ollama.localhost:11434/v1 \
  -e TELEGRAM_TOKEN=12345 \
  -e MODEL_TEXT_REQUEST=llama3.1:8b-instruct-q6_K
  -e MODEL_SUMMARIZE_REQUEST=mistral-nemo:12b-instruct-2407-q4_K_M
  skobkin/telegram-llm-bot
```
