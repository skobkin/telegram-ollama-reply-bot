FROM golang:1-alpine as builder

WORKDIR /build

COPY . .

RUN go build -o app


FROM alpine:latest

WORKDIR /app

COPY --from=builder /build/app .

# Do not forget "/v1" in the end
ENV OPENAI_API_BASE_URL="" \
    OPENAI_API_TOKEN="" \
    TELEGRAM_TOKEN="" \
    MODEL_TEXT_REQUEST="llama3.1:8b-instruct-q6_K" \
    MODEL_SUMMARIZE_REQUEST="llama3.1:8b-instruct-q6_K"

CMD ["/app/app"]
