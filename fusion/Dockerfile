## syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build

WORKDIR /src

RUN apk add --no-cache ca-certificates tzdata

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH

RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
  go build -trimpath -ldflags="-s -w" -o /out/meshtastic-fusion .

FROM gcr.io/distroless/base-debian12

WORKDIR /
COPY --from=build /out/meshtastic-fusion /usr/local/bin/meshtastic-fusion

EXPOSE 9000

ENTRYPOINT ["/usr/local/bin/meshtastic-fusion"]
