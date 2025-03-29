# hadolint global ignore=DL3006
FROM golang:alpine AS builder

WORKDIR /app

COPY go.* ./
RUN go mod download

COPY main.go ./
RUN --mount=type=cache,target=/root/.cache/go-build CGO_ENABLED=0 go build -o sync  .


FROM cgr.dev/chainguard/static

VOLUME /data

COPY --from=builder /app/sync /

ENTRYPOINT ["/sync"]
