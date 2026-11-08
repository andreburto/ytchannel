#!/usr/bin/env bash
set -euo pipefail

BUILD="${1:-}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
IMAGE_TAG="ytchannel:latest"
CONTAINER_NAME="ytchannel"
HOST_DB_FILE="$SCRIPT_DIR/channels.db"
HOST_DB_BACKUP="$SCRIPT_DIR/channels.bak"
CONTAINER_DB_PATH="/app/channel.db"

# Make sure the DB file exists on the host before mounting it, otherwise
# Docker will create a directory in its place.
if [[ ! -f "$HOST_DB_FILE" ]]; then
  touch "$HOST_DB_FILE"
fi

# Keep a single backup of the DB, overwriting any previous backup.
cp -f "$HOST_DB_FILE" "$HOST_DB_BACKUP"

if [[ "$BUILD" == "build" ]]; then
  echo "Building $IMAGE_TAG..."
  docker build -t "$IMAGE_TAG" "$SCRIPT_DIR"
fi

echo "Starting $CONTAINER_NAME..."
docker run -d \
  --name "$CONTAINER_NAME" \
  -p 8000:8000 \
  -e DB_FILE=channel.db \
  --mount type=bind,source="$HOST_DB_FILE",target="$CONTAINER_DB_PATH" \
  "$IMAGE_TAG"

echo "$CONTAINER_NAME is running."
