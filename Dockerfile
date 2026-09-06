FROM node:22-alpine AS web
WORKDIR /src
COPY package.json package-lock.json ./
RUN npm ci
COPY apps/web ./apps/web
RUN npx vite build --config apps/web/vite.config.ts

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY --from=web /src/internal/webui/dist ./internal/webui/dist
ARG VERSION
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/omni-api ./cmd/omni-api

FROM alpine:3.22
RUN adduser -D -H -u 10001 omni \
	&& apk add --no-cache ca-certificates \
	&& install -d -o omni -g omni -m 0700 /data
COPY --from=build /out/omni-api /usr/local/bin/omni-api
USER omni
EXPOSE 47831
VOLUME ["/data"]
ENTRYPOINT ["/usr/local/bin/omni-api"]
CMD ["--mode", "server", "--host", "0.0.0.0", "--port", "47831", "--data-dir", "/data"]
