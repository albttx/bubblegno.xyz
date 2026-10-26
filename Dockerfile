# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26.8-alpine AS build
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/bubblegno ./cmd/bubblegno

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/bubblegno /bubblegno
ENV BUBBLEGNO_HTTP_ADDR=:8090
EXPOSE 8090
USER nonroot:nonroot
ENTRYPOINT ["/bubblegno"]
