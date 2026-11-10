FROM golang:1.26.10-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /koplink ./cmd/server
FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata && addgroup -S koplink && adduser -S koplink -G koplink && mkdir -p /app/uploads && chown -R koplink:koplink /app
WORKDIR /app
COPY --from=build /koplink /app/koplink
USER koplink
EXPOSE 8080
CMD ["/app/koplink","serve"]
