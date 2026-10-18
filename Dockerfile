# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /forgecraft ./cmd/forgecraft

# docker:cli bringt docker und das compose-Plugin mit; Forgecraft ruft "docker compose" auf.
FROM docker:29-cli
COPY --from=build /forgecraft /usr/local/bin/forgecraft
ENV FORGECRAFT_DATA=/srv/forgecraft \
    FORGECRAFT_LISTEN=:8420 \
    GOMEMLIMIT=48MiB
EXPOSE 8420
ENTRYPOINT ["/usr/local/bin/forgecraft"]
