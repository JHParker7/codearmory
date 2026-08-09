# Build context: src/systems  (needs ./portal for the SPA source and ./portal-bff
# for the Go server). Build from the monorepo with:
#   docker build -f portal-bff/Dockerfile -t portal-bff src/systems

# 1. Build the React SPA with npm. The SPA still requires the npm/Vite toolchain —
#    this is the only stage that touches npm, and it never reaches the runtime image.
FROM public.ecr.aws/docker/library/node:22-alpine AS build-spa
WORKDIR /spa
COPY portal/package*.json ./
RUN npm ci
COPY portal/src ./src
COPY portal/index.html portal/tsconfig.json portal/vite.config.ts ./
# The sub-path the SPA will be served from, baked into every asset URL by Vite. It must
# match portal-bff's PORTAL_BASE_PATH at runtime — see vite.config.ts. Empty (default)
# builds a root-mounted SPA, so this changes nothing unless a deployment asks for it.
ARG PORTAL_BASE_PATH=""
ENV PORTAL_BASE_PATH=$PORTAL_BASE_PATH
RUN npm run build

# 2. Build the Go BFF binary (standalone module, GOWORK=off — no workspace needed).
FROM public.ecr.aws/docker/library/golang:1.26.0-alpine3.23 AS build-bff
RUN apk add --no-cache git
ENV GONOSUMDB=github.com/code-armory-app
ENV GOWORK=off
# go.mod replaces the SDK with the in-repo copy at ../sdk, so it has to be staged
# before the module cache warm-up. The context is already src/systems for the SPA,
# so this costs nothing extra.
WORKDIR /src
COPY sdk/ ./sdk/
WORKDIR /src/portal-bff
COPY portal-bff/go.mod portal-bff/go.sum ./
RUN go mod download
COPY portal-bff/ .
RUN CGO_ENABLED=0 go build -o portal-bff .

# 3. Minimal runtime: a static Go binary plus the built SPA. No Node, no
#    node_modules — the npm supply chain is absent from the running image.
FROM public.ecr.aws/docker/library/alpine:3.23.3
RUN apk --no-cache add ca-certificates && addgroup -S -g 101 svc && adduser -S -u 100 -G svc svc
WORKDIR /home/svc/
COPY --from=build-bff /src/portal-bff/portal-bff .
COPY --from=build-spa /spa/dist ./public
RUN chown -R svc:svc /home/svc
USER 100:101
ENV PUBLIC_DIR=/home/svc/public
EXPOSE 3001
CMD ["./portal-bff"]
