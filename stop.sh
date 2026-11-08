#!/usr/bin/env bash
set -euo pipefail

CONTAINER_NAME="ytchannel"

if docker ps -a --format '{{.Names}}' | grep -qx "$CONTAINER_NAME"; then
  echo "Stopping $CONTAINER_NAME..."
  docker rm -f "$CONTAINER_NAME"
  echo "$CONTAINER_NAME stopped and removed."
else
  echo "No container named $CONTAINER_NAME found."
fi
