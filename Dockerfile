# Build stage
FROM golang:1.23-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/server ./cmd/server

# Runtime stage
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /
COPY --from=build /out/server /server
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/server"]
