# API Dockerfile for OSS self-host deployments.
#
# Build: docker build -f Dockerfile -t pandastack/api:dev ./api
# Run:   docker run -p 8080:8080 -e PANDASTACK_AGENT_URL=... pandastack/api:dev
#
# Stripe metering is NOT compiled in. To enable, build with:
#   docker build --build-arg GO_BUILD_TAGS=stripe -f Dockerfile -t pandastack/api:cloud ./api

FROM golang:1.25-alpine AS build
WORKDIR /src
RUN apk add --no-cache git ca-certificates
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG GO_BUILD_TAGS=""
RUN CGO_ENABLED=0 GOOS=linux go build \
    -tags "${GO_BUILD_TAGS}" \
    -ldflags "-s -w" \
    -o /out/api ./cmd/api

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/api /api
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/api"]
