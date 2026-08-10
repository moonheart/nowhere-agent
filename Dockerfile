# nowhere-agent — deployable image.
#
# Multi-stage: builds the SPA (web/dist) and the gateway binary, then ships
# both in a slim runtime image. The result is the whole platform in one
# container: HTTP gateway + admin console + agent loop, needing only Postgres
# (and optionally Redis for multi-instance fan-out).

# --- Stage 1: frontend ----------------------------------------------------
FROM node:22-alpine AS web
WORKDIR /web
COPY web/package.json web/pnpm-lock.yaml ./
RUN corepack enable && pnpm install --frozen-lockfile
COPY web/ ./
RUN pnpm build

# --- Stage 2: backend ------------------------------------------------------
FROM golang:1.26 AS gobuild
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
COPY pkg/ pkg/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/migrate ./cmd/migrate \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mockllm ./cmd/mockllm

# --- Stage 3: runtime ------------------------------------------------------
# debian-slim (not alpine): the local sandbox backend shells out to bash, and
# glibc keeps the docker SDK's socket transport predictable.
FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates bash \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=gobuild /out/server /usr/local/bin/server
COPY --from=gobuild /out/migrate /usr/local/bin/migrate
COPY --from=gobuild /out/mockllm /usr/local/bin/mockllm
COPY --from=web /web/dist ./web/dist
COPY migrations/ ./migrations/

ENV WEB_DIR=/app/web/dist
EXPOSE 8080
ENTRYPOINT ["server"]
