# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS builder
WORKDIR /src
ENV GOTOOLCHAIN=auto

RUN apk add --no-cache git ca-certificates

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gvx-ext-github-actions ./cmd/gvx-ext-github-actions

FROM alpine:3.21
RUN apk add --no-cache ca-certificates git bash
WORKDIR /app

COPY --from=builder /out/gvx-ext-github-actions /usr/local/bin/gvx-ext-github-actions

EXPOSE 18900
ENTRYPOINT ["/usr/local/bin/gvx-ext-github-actions"]
