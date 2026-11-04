# syntax=docker/dockerfile:1

# Build stage: full Go toolchain, discarded after the build.
FROM golang:1.27-alpine AS build
WORKDIR /src

# Download modules in their own layer, so code changes do not re-download dependencies.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# CGO off gives a static binary that runs on the minimal runtime image below.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/notifier ./cmd/notifier

# Runtime stage: no shell or package manager, runs as an unprivileged user.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/notifier /notifier
EXPOSE 8080
USER nonroot:nonroot
# Exec form makes the binary PID 1, so it receives SIGTERM from `docker stop` directly.
ENTRYPOINT ["/notifier"]
