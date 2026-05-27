FROM public.ecr.aws/docker/library/golang:1.26.0-alpine3.23 AS builder
RUN apk add --no-cache git
ENV GONOSUMDB=github.com/code-armory-app/codearmory
WORKDIR /src/registry
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o registry .


FROM public.ecr.aws/docker/library/alpine:3.23.3
RUN apk --no-cache add ca-certificates && addgroup -S svc && adduser -S svc -G svc
WORKDIR /home/svc/
COPY --from=builder /src/registry .
RUN chown svc:svc ./registry
USER svc
EXPOSE 8084
CMD ["./registry"]
