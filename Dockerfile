FROM --platform=$BUILDPLATFORM docker.io/nixos/nix:2.35.2 AS builder

COPY . /tmp/build
WORKDIR /tmp/build

ARG BUILDOS
ARG BUILDARCH
ARG TARGETOS
ARG TARGETARCH
ARG VERSION

RUN VERSION="$VERSION" nix \
    --extra-experimental-features "nix-command flakes" \
    --option filter-syscalls false \
    build --impure ".#service-reflector-cross-$TARGETOS-$TARGETARCH"
RUN ln -s ../bin result/bin/"$BUILDOS"_"$BUILDARCH"
FROM scratch

ARG TARGETOS
ARG TARGETARCH

COPY --from=builder /tmp/build/result/bin/"$TARGETOS"_"$TARGETARCH"/service-reflector /service-reflector

ENTRYPOINT ["/service-reflector"]
