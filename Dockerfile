FROM golang:1-alpine AS test
WORKDIR /src
COPY go.mod ./
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 go test -count=1 ./... && go vet ./...

FROM --platform=$BUILDPLATFORM golang:1-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/julie ./cmd/julie

FROM gcr.io/distroless/static:nonroot AS runtime
COPY --from=build /out/julie /julie
WORKDIR /workspace
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/julie"]
CMD ["-listen", ":8080", "-config", "/etc/julie/model.json", "-workspace", "/workspace", "-no-tools"]