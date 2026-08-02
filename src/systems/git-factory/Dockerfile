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
WORKDIR /src/git-factory
COPY git-factory/go.mod git-factory/go.sum ./
RUN go mod download
COPY git-factory/ .
RUN CGO_ENABLED=0 go build -o git-factory .


FROM public.ecr.aws/docker/library/alpine:3.23.3
# git is a RUNTIME dependency, not just a build one: the git plane shells out to
# git/git-upload-pack/git-receive-pack (ARCHITECTURE §1, §5 step 1).
RUN apk --no-cache add ca-certificates git && addgroup -S -g 101 svc && adduser -S -u 100 -G svc svc
WORKDIR /home/svc/
COPY --from=builder /src/git-factory/git-factory .
RUN chown svc:svc ./git-factory
USER 100:101
EXPOSE 9002
CMD ["./git-factory"]
