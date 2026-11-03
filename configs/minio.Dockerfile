# MinIO server image built from the official release binary.
#
# The upstream images (minio/minio on Docker Hub, quay.io/minio/minio) are no
# longer publicly pullable, so docker-compose.yaml expects an image that you
# build locally under the upstream tag with configs/build-minio-image.sh.
#
#   docker build -f configs/minio.Dockerfile -t minio/minio:<release> .
#
# Requires the binary at ./minio in the build context (the script fetches it).
FROM alpine:3.22

# ca-certificates: MinIO serves TLS and talks to IdPs; curl backs the compose
# healthcheck.
RUN apk add --no-cache ca-certificates curl

COPY minio /usr/local/bin/minio

EXPOSE 9000 9001

ENTRYPOINT ["/usr/local/bin/minio"]
