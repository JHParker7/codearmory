FROM public.ecr.aws/docker/library/golang:1.26.0-alpine3.23 AS builder
WORKDIR /src/registry
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o registry .


FROM public.ecr.aws/docker/library/alpine:3.23.3
RUN apk --no-cache add ca-certificates
WORKDIR /root/
COPY --from=builder /src/registry .
EXPOSE 8084
CMD ["./registry"]
