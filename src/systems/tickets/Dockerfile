FROM public.ecr.aws/docker/library/golang:1.26.0-alpine3.23 AS builder
RUN apk add --no-cache git
ENV GONOSUMDB=github.com/code-armory-app
WORKDIR /src
COPY --from=sdk . /src/codearmory_sdk
RUN printf 'go 1.26\n\nuse (\n\t./codearmory_sdk\n\t./tickets\n)\n' > /src/go.work
WORKDIR /src/tickets
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o tickets .


FROM public.ecr.aws/docker/library/alpine:3.23.3
RUN apk --no-cache add ca-certificates && addgroup -S svc && adduser -S svc -G svc
WORKDIR /home/svc/
COPY --from=builder /src/tickets/tickets .
RUN chown svc:svc ./tickets
USER svc
EXPOSE 8086
CMD ["./tickets"]
