FROM golang:1.25-alpine AS build

WORKDIR /app

COPY go.mod go.sum ./

RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o worker .

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y ffmpeg ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /app/worker /usr/local/bin/worker

ENTRYPOINT ["worker"]