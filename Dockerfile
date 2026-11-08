FROM golang:1-alpine AS test
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 go test -count=1 ./... && go vet ./...

FROM --platform=$BUILDPLATFORM golang:1-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/julie ./cmd/julie && \
	CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/julie-mcp ./cmd/julie-mcp

FROM gcr.io/distroless/static:nonroot AS runtime
COPY --from=build /out/julie /julie
COPY --from=build /out/julie-mcp /julie-mcp
WORKDIR /workspace
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/julie"]
CMD ["-listen", ":8080", "-config", "/etc/julie/model.json", "-workspace", "/workspace", "-no-tools"]