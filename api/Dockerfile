# --- build stage ---
FROM golang:1.22-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOOS=linux GOFLAGS=-mod=mod
COPY go.mod ./
COPY *.go ./
# Resolves deps and generates go.sum in-image (needs network at build time).
RUN go mod tidy && go build -ldflags="-s -w" -trimpath -o /out/windep-api .

# --- runtime stage (distroless, non-root, static) ---
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/windep-api /windep-api
USER nonroot:nonroot
EXPOSE 8443
ENTRYPOINT ["/windep-api"]
