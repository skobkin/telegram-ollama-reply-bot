# Upgrade Notes

## 2026-04-19 - Privacy-first in-memory state layer introduced

### State Config Migration

The old history size variable was removed.

Replace:

- `BOT_HISTORY_LENGTH` -> `STATE_HISTORY_MESSAGES_PER_STREAM`

Add explicit state limits as needed:

- `STATE_MAX_BYTES`
- `STATE_HISTORY_MAX_BYTES`
- `STATE_HISTORY_STREAMS_MAX`
- `STATE_IMAGE_CACHE_MAX_BYTES`
- `STATE_IMAGE_CACHE_TTL`

### Example

Old:

```env
BOT_HISTORY_LENGTH=150
```

New:

```env
STATE_HISTORY_MESSAGES_PER_STREAM=150
STATE_HISTORY_STREAMS_MAX=1024
STATE_HISTORY_MAX_BYTES=167772160
STATE_IMAGE_CACHE_TTL=24h
```


## 2026-04-18 - Backend abstractions and Ollama backend introduced

### Backend Config Migration

The old flat LLM environment variables were removed.

Replace:

- `OPENAI_API_BASE_URL` -> `LLM_BACKEND_OPENAI_COMPAT_BASE_URL`
- `OPENAI_API_TOKEN` -> `LLM_BACKEND_OPENAI_COMPAT_API_TOKEN`
- `MODEL_TEXT_REQUEST` -> `LLM_FEATURE_CHAT_MODEL`
- `MODEL_SUMMARIZE_REQUEST` -> `LLM_FEATURE_SUMMARIZE_MODEL`
- `MODEL_IMAGE_RECOGNITION` -> `LLM_FEATURE_IMAGE_RECOGNITION_MODEL`

### New Required Routing

Set at least:

- `LLM_FEATURE_CHAT_MODEL`
- `LLM_FEATURE_CHAT_BACKEND` if you want something other than the default `openai_compat`

By default, summarize, image recognition, and future tool use inherit the chat backend/model unless explicitly overridden.

### OpenAI-Compatible Backend

If you use an OpenAI-compatible endpoint, set:

- `LLM_BACKEND_OPENAI_COMPAT_BASE_URL`
- `LLM_BACKEND_OPENAI_COMPAT_API_TOKEN`

Leave feature backends on `openai_compat` or set them explicitly.

### Ollama Native Backend

If you use Ollama native API, set:

- `LLM_BACKEND_OLLAMA_BASE_URL`
- `LLM_FEATURE_CHAT_BACKEND=ollama`
- `LLM_FEATURE_CHAT_MODEL=<your model>`

Optionally override:

- `LLM_FEATURE_SUMMARIZE_BACKEND`
- `LLM_FEATURE_SUMMARIZE_MODEL`
- `LLM_FEATURE_IMAGE_RECOGNITION_BACKEND`
- `LLM_FEATURE_IMAGE_RECOGNITION_MODEL`
- `LLM_FEATURE_TOOL_USE_BACKEND`
- `LLM_FEATURE_TOOL_USE_MODEL`

### Example

Old:

```env
OPENAI_API_BASE_URL=http://ollama.localhost:11434/v1
OPENAI_API_TOKEN=dummy
MODEL_TEXT_REQUEST=gemma3:27b
MODEL_SUMMARIZE_REQUEST=gemma3:12b
MODEL_IMAGE_RECOGNITION=gemma3:12b
```

New:

```env
LLM_BACKEND_OLLAMA_BASE_URL=http://ollama.localhost:11434
LLM_FEATURE_CHAT_BACKEND=ollama
LLM_FEATURE_CHAT_MODEL=gemma3:27b
LLM_FEATURE_SUMMARIZE_MODEL=gemma3:12b
LLM_FEATURE_IMAGE_RECOGNITION_MODEL=gemma3:12b
```
