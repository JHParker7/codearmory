FROM public.ecr.aws/docker/library/golang:1.26.0-alpine3.23 AS builder
RUN apk add --no-cache git
ENV GONOSUMDB=github.com/code-armory-app
WORKDIR /src/blueprints
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o blueprints .

FROM public.ecr.aws/docker/library/alpine:3.23.3
RUN apk --no-cache add ca-certificates && addgroup -S -g 101 svc && adduser -S -u 100 -G svc svc
WORKDIR /home/svc/
COPY --from=builder /src/blueprints/blueprints .
RUN chown svc:svc ./blueprints
USER 100:101
EXPOSE 8084
CMD ["./blueprints"]
