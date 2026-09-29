# Стадия сборки
FROM golang:1.25-alpine AS build

WORKDIR /app

# Зависимости копируются отдельным слоем: он пересобирается только при
# изменении go.mod / go.sum.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Статическая сборка без CGO: бинарник работает в минимальном образе.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/main ./server

# Стадия выполнения
FROM alpine:3.20

RUN apk --no-cache add ca-certificates \
    && addgroup -S app \
    && adduser -S -G app app

WORKDIR /app

COPY --from=build /out/main .

USER app

EXPOSE 8080

HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:8080/health || exit 1

CMD ["./main"]
