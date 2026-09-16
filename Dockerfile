FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN go test ./... && \
    go build -buildvcs=false -trimpath -o /out/catpaw2api ./cmd/server && \
    go build -buildvcs=false -trimpath -o /out/catpaw2api-login ./cmd/login && \
    go build -buildvcs=false -trimpath -o /out/catpaw2api-credit ./cmd/credit && \
    go build -buildvcs=false -trimpath -o /out/catpaw2api-apply ./cmd/apply

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=build /out/ /usr/local/bin/
COPY config.example.json ./config.example.json
VOLUME ["/app/auths", "/app/data"]
EXPOSE 7867
CMD ["catpaw2api", "-config", "config.json"]
