# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS builder
ARG TARGETOS TARGETARCH VERSION=dev COMMIT=unknown
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" -o /out/genmon-guard .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/genmon-guard /genmon-guard
USER 65532:65532
WORKDIR /var/lib/genmon-guard/
EXPOSE 8080
ENTRYPOINT ["/genmon-guard"]
CMD ["-config=/etc/genmon-guard/config.yaml"]
