FROM golang:1.24.13 as builder

WORKDIR /workspace

COPY go.mod go.mod
COPY go.sum go.sum

RUN go mod download

COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/
COPY hack/ hack/

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -ldflags="-w -s" -o manager ./cmd/manager
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -ldflags="-w -s" -o listener ./cmd/listener

FROM gcr.io/distroless/base-debian12:nonroot

WORKDIR /

COPY --from=builder /workspace/manager .
COPY --from=builder /workspace/listener .

EXPOSE 8080 8081

# Default to manager; override with listener as needed
ENTRYPOINT ["/manager"]
