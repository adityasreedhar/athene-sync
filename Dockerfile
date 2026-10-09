# Athene sync server: one small static program in an otherwise empty image, for x86 and ARM
# (a Raspberry Pi runs it). Go compiles for each machine on the machine building, so one
# BuildKit build makes all three without emulating them:
#   docker buildx build --platform linux/amd64,linux/arm64,linux/arm/v7 -t athenereader/athene-sync .
# An older builder without BuildKit builds for x86.
FROM --platform=${BUILDPLATFORM:-linux/amd64} golang:1.25-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} GOARM=${TARGETVARIANT#v} \
    go build -trimpath -ldflags="-s -w" -o /out/athene-sync . && mkdir -p /out/data

FROM scratch
COPY --from=build /out/athene-sync /athene-sync
COPY --from=build /out/data /data
ENV ATHENE_DATA=/data ATHENE_LISTEN=:17200
VOLUME /data
EXPOSE 17200
HEALTHCHECK --interval=60s --timeout=5s --start-period=10s CMD ["/athene-sync", "-healthcheck"]
ENTRYPOINT ["/athene-sync"]
