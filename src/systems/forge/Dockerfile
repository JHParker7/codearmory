FROM public.ecr.aws/docker/library/golang:1.26.0-alpine3.23 AS builder
RUN apk add --no-cache git
ENV GONOSUMDB=github.com/code-armory-app/codearmory
WORKDIR /src/forge
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o forge .

FROM public.ecr.aws/docker/library/alpine:3.23.3
RUN apk --no-cache add ca-certificates && addgroup -S svc && adduser -S svc -G svc
WORKDIR /home/svc/
COPY --from=builder /src/forge/forge .
RUN chown svc:svc ./forge
USER svc
EXPOSE 8083
CMD ["./forge"]
