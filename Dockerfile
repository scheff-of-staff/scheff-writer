# syntax=docker/dockerfile:1
# Multi-stage: static binary on distroless, non-root. Configuration is
# environment-only at run time; nothing secret is baked in.

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/scheff-writer ./cmd/scheff-writer

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/scheff-writer /scheff-writer
ENV WRITER_LISTEN=:8445
EXPOSE 8445
USER nonroot:nonroot
ENTRYPOINT ["/scheff-writer"]
