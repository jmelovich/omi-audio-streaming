FROM golang:1.22.1-alpine AS builder
WORKDIR /app
COPY . .
RUN go mod download
RUN go build -o ./omi-audio-streaming ./main.go
 
 
FROM alpine:latest AS runner
WORKDIR /app
COPY --from=builder /app/omi-audio-streaming .
EXPOSE 8080
ENTRYPOINT ["./omi-audio-streaming"]