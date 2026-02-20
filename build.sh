#!/usr/bin/env bash
set -euo pipefail

# Build the byoc-gateway-proxy Docker image.
#
# Examples:
#   ./build.sh                                              # local build, tag: latest
#   TAG=v1.0.0 ./build.sh                                   # local build, custom tag
#   REGISTRY=myregistry.example.com TAG=v1.0.0 ./build.sh   # with registry prefix
#   REGISTRY=myregistry.example.com PUSH=true ./build.sh    # build and push

TAG="${TAG:-latest}"
PUSH="${PUSH:-false}"
REGISTRY="${REGISTRY:-}"

if [ -n "$REGISTRY" ]; then
  IMAGE="${REGISTRY}/livepeer-byoc-gateway-proxy:${TAG}"
else
  IMAGE="livepeer-byoc-gateway-proxy:${TAG}"
fi

echo "==> Building ${IMAGE}"
docker build -t "$IMAGE" .

echo ""
echo "Image built successfully: ${IMAGE}"

if [ "$PUSH" = "true" ]; then
  echo ""
  echo "==> Pushing ${IMAGE}"
  docker push "$IMAGE"
  echo ""
  echo "Image pushed successfully."
fi
