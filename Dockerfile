FROM golang:1.26-alpine AS builder

WORKDIR /build

COPY . .

RUN go build -trimpath -o /tmp/app ./cmd/bot


FROM alpine:3.23

WORKDIR /app

COPY --from=builder /tmp/app .

VOLUME ["/data"]

ENV LLM_BACKEND_OPENAI_COMPAT_BASE_URL="" \
    LLM_BACKEND_OPENAI_COMPAT_API_TOKEN="" \
    TELEGRAM_TOKEN="" \
    PERSISTENT_STORE_PATH="/data/db.sqlite" \
    LOG_LEVEL="info" \
    LLM_FEATURE_CHAT_MODEL="gemma3:12b" \
    LLM_FEATURE_SUMMARIZE_MODEL="gemma3:12b" \
    LLM_FEATURE_IMAGE_RECOGNITION_MODEL="gemma3:12b"

CMD ["/app/app"]
