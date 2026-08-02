FROM public.ecr.aws/docker/library/golang:1.26.0-alpine3.23 AS builder
RUN apk add --no-cache git
ENV GONOSUMDB=github.com/code-armory-app
# Build context is src/systems, NOT this service directory: go.mod carries a
# replace pointing at the in-repo SDK (../sdk), so the build needs both trees.
# GOWORK=off because go.work is deliberately not copied — workspace mode would
# demand every member module be present, and only this service and the SDK are.
ENV GOWORK=off
WORKDIR /src
COPY sdk/ ./sdk/
WORKDIR /src/gatekeeper
COPY gatekeeper/go.mod gatekeeper/go.sum ./
RUN go mod download

COPY gatekeeper/ .
RUN CGO_ENABLED=0 go build -o gatekeeper .


FROM public.ecr.aws/docker/library/alpine:3.23.3
RUN apk --no-cache add ca-certificates && addgroup -S -g 101 svc && adduser -S -u 100 -G svc svc
WORKDIR /home/svc/
COPY --from=builder /src/gatekeeper/gatekeeper .
RUN chown svc:svc ./gatekeeper
USER 100:101
EXPOSE 8081
CMD ["./gatekeeper"]
