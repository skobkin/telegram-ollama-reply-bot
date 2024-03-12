# telegram-ollama-reply-bot

[![Build Status](https://ci.skobk.in/api/badges/skobkin/telegram-ollama-reply-bot/status.svg)](https://ci.skobk.in/skobkin/telegram-ollama-reply-bot)

# Usage

## Docker

```shell
docker run \
  -e OLLAMA_TOKEN=123 \
  -e OLLAMA_BASE_URL=http://ollama.localhost:11434/v1 \
  -e TELEGRAM_TOKEN=12345 \
  skobkin/telegram-llm-bot
```
