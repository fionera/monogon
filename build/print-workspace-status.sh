#!/usr/bin/env bash

# Workspace status used for build stamping.
set -o errexit
set -o nounset
set -o pipefail

# TODO: Figure out how to version Metropolis
METROPOLIS_VERSION=0.1

KUBERNETES_gitTreeState="clean"
if [ ! -z "$(git status --porcelain)" ]; then
    KUBERNETES_gitTreeState="dirty"
fi

# TODO(q3k): unify with //third_party/go/repsitories.bzl.
KUBERNETES_gitMajor="1"
KUBERNETES_gitMinor="24"
KUBERNETES_gitVersion="v1.24.2+mngn"

# CI doesnt have the user set...
IMAGE_TAG=${USER:-unknown}-$(date +%s)

cat <<EOF
KUBERNETES_gitCommit $(git rev-parse "HEAD^{commit}")
KUBERNETES_gitTreeState $KUBERNETES_gitTreeState
STABLE_KUBERNETES_gitVersion $KUBERNETES_gitVersion
STABLE_KUBERNETES_gitMajor $KUBERNETES_gitMajor
STABLE_KUBERNETES_gitMinor $KUBERNETES_gitMinor
KUBERNETES_buildDate $(date \
  ${SOURCE_DATE_EPOCH:+"--date=@${SOURCE_DATE_EPOCH}"} \
 -u +'%Y-%m-%dT%H:%M:%SZ')
STABLE_METROPOLIS_version $METROPOLIS_VERSION
IMAGE_TAG $IMAGE_TAG
EOF
