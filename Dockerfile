FROM golang:1.26-alpine AS builder

WORKDIR /build

COPY . .

ARG VERSION=dev

RUN go build -trimpath -ldflags="-s -w -X 'telegram-ollama-reply-bot/internal/buildinfo.Version=${VERSION}'" -o /tmp/app ./cmd/bot


FROM alpine:3.23

WORKDIR /app

COPY --from=builder /tmp/app .

VOLUME ["/data"]

ENV LLM__BACKENDS__OPENAI_COMPAT__BASE_URL="" \
    LLM__BACKENDS__OPENAI_COMPAT__API_TOKEN="" \
    BOT__TELEGRAM__TOKEN="" \
    PERSISTENT__STORE_PATH="/data/db.sqlite" \
    LOG__LEVEL="info"

CMD ["/app/app"]
