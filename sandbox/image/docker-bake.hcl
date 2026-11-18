# docker buildx bake definitions for the sandbox image (plan §6.2).
# Run from the repository root:
#   docker buildx bake -f sandbox/image/docker-bake.hcl sandbox-local   # load headmesh-sandbox:dev
#   docker buildx bake -f sandbox/image/docker-bake.hcl sandbox-ci      # load for tests in CI
#   VERSION=0.1.0 docker buildx bake -f sandbox/image/docker-bake.hcl sandbox --push

variable "VERSION" {
  default = "dev"
}

variable "REVISION" {
  default = "unknown"
}

variable "CREATED" {
  default = "1970-01-01T00:00:00Z"
}

variable "IMAGE" {
  default = "ghcr.io/headmesh-dev/sandbox"
}

variable "AGENT_UID" {
  default = "1000"
}

variable "AGENT_GID" {
  default = "1000"
}

group "default" {
  targets = ["sandbox-local"]
}

target "_common" {
  context    = "sandbox"
  dockerfile = "image/Dockerfile"
  platforms  = ["linux/amd64"]
  args = {
    VERSION   = VERSION
    REVISION  = REVISION
    CREATED   = CREATED
    AGENT_UID = AGENT_UID
    AGENT_GID = AGENT_GID
  }
  # GHCR reads the index annotations when attestations make a build an OCI
  # index, even when the image contains only one runnable architecture.
  annotations = [
    "index,manifest:org.opencontainers.image.title=Headmesh agent sandbox",
    "index,manifest:org.opencontainers.image.description=Persistent agent sandbox that joins a Headscale tailnet as its own node",
    "index,manifest:org.opencontainers.image.source=https://github.com/headmesh-dev/headmesh",
    "index,manifest:org.opencontainers.image.documentation=https://github.com/headmesh-dev/headmesh/tree/main/sandbox",
    "index,manifest:org.opencontainers.image.url=https://headmesh.dev",
    "index,manifest:org.opencontainers.image.vendor=Xcelsior Compute Inc.",
    "index,manifest:org.opencontainers.image.licenses=Apache-2.0",
    "index,manifest:org.opencontainers.image.version=${VERSION}",
    "index,manifest:org.opencontainers.image.revision=${REVISION}",
    "index,manifest:org.opencontainers.image.created=${CREATED}",
  ]
  # Optional: a token lifts GitHub API rate limits for mise downloads.
  secret = ["type=env,id=github_token,env=GITHUB_TOKEN"]
}

# Local development build, loaded into the Docker image store.
target "sandbox-local" {
  inherits = ["_common"]
  tags     = ["headmesh-sandbox:${VERSION}"]
  output   = ["type=docker"]
}

# CI test build: loaded so hadolint, container-structure-test and trivy can run
# before anything is pushed. It deliberately avoids the private release cache,
# so this job does not need package-read permission.
target "sandbox-ci" {
  inherits = ["_common"]
  tags     = ["headmesh-sandbox:ci"]
  output   = ["type=docker"]
}

# Release build: pushed to GHCR with SBOM and provenance attestations, then
# signed by the workflow with cosign (keyless).
target "sandbox" {
  inherits   = ["_common"]
  tags       = ["${IMAGE}:${VERSION}", "${IMAGE}:sha-${REVISION}"]
  cache-from = ["type=registry,ref=${IMAGE}:buildcache"]
  cache-to   = ["type=registry,ref=${IMAGE}:buildcache,mode=max"]
  attest     = ["type=provenance,mode=max", "type=sbom"]
  output     = ["type=registry"]
}
