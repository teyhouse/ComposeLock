# syntax=docker/dockerfile:1
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
RUN CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=$VERSION -X main.commit=$COMMIT -X main.date=$DATE" \
    -o /out/composelock ./cmd/composelock

FROM cgr.dev/chainguard/wolfi-base:latest
RUN apk add --no-cache git ca-certificates
COPY --from=build /out/composelock /usr/local/bin/composelock
ENTRYPOINT ["composelock"]
CMD ["poll"]
