#!/bin/bash
set -euo pipefail

# Our local user needs write access to /dev/kvm (best accomplished by
# adding your user to the kvm group).
if ! touch /dev/kvm; then
  echo "Cannot write to /dev/kvm - please verify permissions."
  exit 1
fi

# The KVM module needs to be loaded, since our container is unprivileged
# and won't be able to do it itself.
if ! [[ -d /sys/module/kvm ]]; then
  echo "kvm module not loaded - please modprobe kvm"
  exit 1
fi

# Rebuild base image
podman build -t nexantic-builder build

# Set up SELinux contexts to prevent the container from writing to
# files that would allow for easy breakouts via tools ran on the host.
chcon -Rh system_u:object_r:container_file_t:s0 .

# Ignore errors - these might already be masked, like when synchronizing the source.
! chcon -Rh unconfined_u:object_r:user_home_t:s0 \
  .arcconfig .idea .git

# Keep this in sync with ci.sh:

podman pod create --name nexantic

# Mount bazel root to identical paths inside and outside the container.
# This caches build state even if the container is destroyed, and
BAZEL_ROOT=${HOME}/.cache/bazel-nxt

# The Bazel plugin injects a Bazel repository into the sync command line.
# Look up the latest copy of it in either the user config folder
# or the Jetbrains Toolbox dir (for non-standard setups).
ASPECT_PATH=$(
  dirname $(
    find ~/.IntelliJIdea*/config/plugins/ijwb/* ~/.local/share/JetBrains \
    -name intellij_info_impl.bzl -printf "%T+\t%p\n" | sort | tail -n 1))

mkdir -p ${BAZEL_ROOT}

podman run -it -d \
    -v $(pwd):$(pwd) \
    -w $(pwd) \
    --volume=${BAZEL_ROOT}:${BAZEL_ROOT} \
    -v ${ASPECT_PATH}:${ASPECT_PATH}:ro \
    --device /dev/kvm \
    --privileged \
    --pod nexantic \
    --name=nexantic-dev \
    nexantic-builder

podman run -it -d \
    --pod nexantic \
    --ulimit nofile=262144:262144 \
    --name=nexantic-cockroach \
    cockroachdb/cockroach:v19.1.5 start --insecure
