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
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/migrate ./cmd/migrate \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mockllm ./cmd/mockllm

# --- Stage 3: runtime ------------------------------------------------------
# debian-slim (not alpine): the local sandbox backend shells out to bash, and
# glibc keeps the docker SDK's socket transport predictable.
FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates bash \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --system --uid 10001 --create-home --home-dir /app app
WORKDIR /app
COPY --from=gobuild /out/server /usr/local/bin/server
COPY --from=gobuild /out/migrate /usr/local/bin/migrate
COPY --from=gobuild /out/mockllm /usr/local/bin/mockllm
COPY --from=web /web/dist ./web/dist
COPY migrations/ ./migrations/

ENV WEB_DIR=/app/web/dist
# The only mutable paths the server can touch: WORKSPACE_DIR (session images +
# local sandbox workspaces; unset by default, which keeps image uploads and
# the local sandbox OFF and all durable state in Postgres) and LLM_RAW_LOG_DIR
# (raw LLM capture; unset = off). /app/workspace exists and is owned by the
# app user so `docker run --read-only` deployments work out of the box for
# the default unset-config; operators who DO set WORKSPACE_DIR or
# LLM_RAW_LOG_DIR must mount writable volumes there.
RUN mkdir -p /app/workspace && chown -R app:app /app
VOLUME ["/app/workspace"]

# Hardening: run as an unprivileged user (uid 10001), never root. Drop the
# remaining Linux capabilities at runtime (not expressible in the image):
#   docker run --cap-drop=ALL --security-opt no-new-privileges:true --read-only
# The docker sandbox backend additionally needs the docker socket mounted and
# the app user admitted to its group.
USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["server"]
