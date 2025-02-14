FROM golang:1.23-alpine as builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod tidy

COPY . .

RUN go build -o proxy-app proxy.go

FROM alpine:latest

WORKDIR /root/

COPY --from=builder /app/proxy-app .

EXPOSE ${LISTEN:-8080}

CMD ["/bin/sh", "-c", "./proxy-app -listen $LISTEN -servers $SERVERS -logdir /logs"]
