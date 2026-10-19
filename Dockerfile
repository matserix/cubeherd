# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X cubeherd/internal/web.Version=${VERSION#v}" -o /cubeherd ./cmd/cubeherd

# docker:cli ships docker and the compose plugin; Cubeherd calls "docker compose".
FROM docker:29-cli
COPY --from=build /cubeherd /usr/local/bin/cubeherd
COPY LICENSE THIRD_PARTY_NOTICES.md /usr/share/doc/cubeherd/
ENV CUBEHERD_DATA=/srv/cubeherd \
    CUBEHERD_LISTEN=:8420 \
    GOMEMLIMIT=48MiB
EXPOSE 8420
ENTRYPOINT ["/usr/local/bin/cubeherd"]
