FROM public.ecr.aws/docker/library/golang:1.26.0-alpine3.23 AS builder
WORKDIR /src/forge
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o forge .

FROM public.ecr.aws/docker/library/alpine:3.23.3
RUN apk --no-cache add ca-certificates
WORKDIR /root/
COPY --from=builder /src/forge/forge .
EXPOSE 8083
CMD ["./forge"]
