# Multi-stage Dockerfile to build Vikunja with the Maps & Location Plugin
# No local Go or Node.js toolchain required on the host.

ARG VIKUNJA_REF=main

# Stage 1: Build Frontend with Leaflet and Maps Components
FROM node:24-alpine AS frontend-builder
WORKDIR /app
ARG VIKUNJA_REF
RUN apk add --no-cache git

# Clone Vikunja base source
RUN git clone --depth 1 --branch ${VIKUNJA_REF} https://github.com/go-vikunja/vikunja.git .

# Apply maps patch and copy plugin
COPY vikunja-core-maps.patch /tmp/vikunja-core-maps.patch
RUN git apply /tmp/vikunja-core-maps.patch
COPY plugins/maps/ /app/plugins/maps/

WORKDIR /app/frontend
RUN npm install -g corepack && corepack enable && \
    pnpm install && \
    pnpm add leaflet@^1.9.4 && \
    pnpm add -D @types/leaflet@^1.9.14 && \
    pnpm run build

# Stage 2: Build Static Vikunja Go Binary
FROM golang:1.24-alpine AS backend-builder
WORKDIR /app
RUN apk add --no-cache git build-base
COPY --from=frontend-builder /app /app
RUN go build -tags 'osusergo netgo' -ldflags "-s -w -extldflags '-static'" -o /app/vikunja .

# Stage 3: Minimal Production Runtime
FROM alpine:latest
RUN apk add --no-cache ca-certificates tzdata curl gcompat
WORKDIR /app/vikunja

RUN addgroup -g 1000 vikunja && \
    adduser -D -u 1000 -G vikunja vikunja && \
    mkdir -p /app/vikunja/files /app/vikunja/plugins

COPY --from=backend-builder --chown=vikunja:vikunja /app/vikunja /app/vikunja/vikunja
COPY --from=backend-builder --chown=vikunja:vikunja /app/plugins/ /app/vikunja/plugins/

RUN chmod +x /app/vikunja/vikunja && \
    chown -R vikunja:vikunja /app/vikunja

USER 1000:1000

ENV VIKUNJA_SERVICE_ROOTPATH=/app/vikunja/
ENV VIKUNJA_FILES_BASEPATH=/app/vikunja/files
VOLUME ["/app/vikunja/files"]
EXPOSE 3456

HEALTHCHECK --interval=15s --timeout=5s --start-period=5s --retries=3 \
  CMD wget -q --spider http://localhost:3456/api/v1/info || exit 1

ENTRYPOINT ["/app/vikunja/vikunja"]
