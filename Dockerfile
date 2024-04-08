# Build the manager binary
FROM europe-docker.pkg.dev/kyma-project/prod/external/golang:1.22.0-alpine3.19 as builder

WORKDIR /workspace

COPY go.mod go.mod
COPY go.sum go.sum

RUN go mod download

COPY certificates/certificates.go certificates/certificates.go
COPY main.go main.go

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -o cert-gen main.go

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/cert-gen .

USER 65532:65532

ENTRYPOINT ["/cert-gen"]
