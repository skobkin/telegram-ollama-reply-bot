FROM golang:1 as builder

WORKDIR /build

COPY . .

RUN go build -o app


FROM alpine:latest

WORKDIR /app

COPY --from=builder /build/app .

CMD ["/app/app"]
