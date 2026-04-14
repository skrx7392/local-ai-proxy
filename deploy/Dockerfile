FROM golang:1.26-alpine AS builder
ARG GIT_SHA=unknown
ARG BUILD_TIME=unknown
WORKDIR /src
COPY go.mod ./
COPY . .
RUN go mod tidy
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X main.version=${GIT_SHA} -X main.buildTime=${BUILD_TIME}" \
    -o /proxy ./cmd/proxy

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /proxy /proxy
EXPOSE 8080
ENTRYPOINT ["/proxy"]
