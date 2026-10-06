FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/whatsapp-go-api .

FROM alpine:3.22
RUN apk add --no-cache ca-certificates && adduser -D -H -u 10001 app
WORKDIR /app
COPY --from=build /out/whatsapp-go-api /usr/local/bin/whatsapp-go-api
USER app
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s CMD wget -q -O /dev/null http://127.0.0.1:8080/readyz || exit 1
ENTRYPOINT ["whatsapp-go-api"]
