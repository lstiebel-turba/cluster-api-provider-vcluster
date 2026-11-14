# One image per architecture, built natively (CI: a runner per architecture, the manifest list
# merged afterwards). BUILDPLATFORM builds, TARGETARCH is what runs.
FROM --platform=$BUILDPLATFORM golang:1.26 AS builder

ARG TARGETOS=linux
ARG TARGETARCH

WORKDIR /workspace

# Helm 3, checksum-verified (the helm installer runs it)
ARG HELM_VERSION=v3.22.1
ARG HELM_SHA256_amd64=896cf63684a42a723d61c980369d6e588330927deb13676ca0d85006dd6da0ea
ARG HELM_SHA256_arm64=4c29e286b279d170fa048ddfc3e4e83dcd069ca9c52102766ba672d7149e694d
RUN set -eu; \
    case "$TARGETARCH" in amd64) sum="$HELM_SHA256_amd64";; arm64) sum="$HELM_SHA256_arm64";; *) echo "no helm checksum for $TARGETARCH" >&2; exit 1;; esac; \
    curl -fsSL "https://get.helm.sh/helm-${HELM_VERSION}-${TARGETOS}-${TARGETARCH}.tar.gz" -o helm.tar.gz; \
    echo "$sum  helm.tar.gz" | sha256sum -c -; \
    tar -xzf helm.tar.gz "${TARGETOS}-${TARGETARCH}/helm"; \
    mv "${TARGETOS}-${TARGETARCH}/helm" helm; \
    rm -r helm.tar.gz "${TARGETOS}-${TARGETARCH}"

COPY go.mod go.sum ./
COPY vendor/ vendor/
COPY main.go ./
COPY api/ api/
COPY controllers/ controllers/
COPY pkg/ pkg/

RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -mod=vendor -trimpath -ldflags="-s -w" -o manager .

FROM gcr.io/distroless/static:nonroot
WORKDIR /

COPY --from=builder /workspace/manager .
COPY --from=builder /workspace/helm .

USER 65532:65532

ENTRYPOINT ["/manager"]
