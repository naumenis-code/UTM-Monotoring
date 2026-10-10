## Build stage
FROM golang:1.26-alpine AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# CGO_ENABLED=0 works because modernc.org/sqlite is a pure-Go SQLite driver.
RUN CGO_ENABLED=0 go build -o /out/UTM-Monotoring .

## Runtime stage
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=builder /out/UTM-Monotoring ./UTM-Monotoring

ENV HTTP_ADDR=:8088
ENV DB_PATH=/data/UTM-Monotoring.db
VOLUME ["/data"]
EXPOSE 8088

ENTRYPOINT ["./UTM-Monotoring"]
