FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/dozor ./cmd/dozor

# The SQLite driver is pure Go, so the binary runs on a bare image.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/dozor /dozor
WORKDIR /data
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["/dozor"]
CMD ["serve", "-config", "/data/dozor.yaml", "-db", "/data/dozor.db", "-addr", ":8080"]
