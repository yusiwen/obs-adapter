# obs-adapter

[![ci](https://github.com/yusiwen/obs-adapter/actions/workflows/ci.yml/badge.svg)](https://github.com/yusiwen/obs-adapter/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/release/yusiwen/obs-adapter?style=flat-square)](https://github.com/yusiwen/obs-adapter/releases)
[![ghcr](https://img.shields.io/badge/ghcr.io-obs--adapter-2496ed?style=flat-square&logo=docker&logoColor=white)](https://github.com/yusiwen/obs-adapter/pkgs/container/obs-adapter)
[![Go version](https://img.shields.io/github/go-mod/go-version/yusiwen/obs-adapter?style=flat-square&logo=go&logoColor=white)](go.mod)
[![Backend: MinIO](https://img.shields.io/badge/backend-MinIO-c72c48?style=flat-square&logo=minio&logoColor=white)](https://min.io)
[![Protocol: OBS](https://img.shields.io/badge/protocol-OBS%20API-0b6cbf?style=flat-square)](docs/protocol.md)
[![Dependencies: stdlib only](https://img.shields.io/badge/dependencies-stdlib%20only-success?style=flat-square)](go.mod)
[![Status: local test verification](https://img.shields.io/badge/status-local%20test%20verification-blueviolet?style=flat-square)](#)
[![Last commit](https://img.shields.io/github/last-commit/yusiwen/obs-adapter?style=flat-square)](https://github.com/yusiwen/obs-adapter/commits/main)

A Go implementation of the **OBS server-side API**: it exposes an endpoint that
looks exactly like Huawei Cloud OBS and stores the objects in MinIO.

The goal is a **local deployment used for test verification**: a legacy Java
application that is hard-wired to the OBS SDK can exercise its full object
storage path in a lab environment, backed by MinIO, with **zero changes on the
Java side** (`new ObsClient(ak, sk, endpoint)` and
`putObject/getObject/deleteObject` keep working as-is).

```
┌──────────────────┐   OBS protocol          ┌──────────────┐   AWS SigV4     ┌─────────┐
│ esdk-obs-java    │  OBS AK:sig(HMAC-SHA1)  │ obs-adapter  │  AK:sig(SHA256) │         │
│ (legacy app)     │ ──────────────────────> │    (Go)      │ ──────────────> │  MinIO  │
│ virtual-hosted   │ <────────────────────── │              │ <────────────── │         │
└──────────────────┘   x-obs-* responses     └──────────────┘   S3 XML/headers└─────────┘
```

## Why a proxy that re-signs, instead of reimplementing on `minio-go`

| Approach | Gains | Costs |
|---|---|---|
| **This one**: accept the OBS protocol, translate headers and signature, forward to the MinIO S3 endpoint | Range, conditional requests, multipart, listing, versioning and copies come for free; small code base; never lags behind MinIO features | You own the OBS signature verification and the SigV4 re-signing |
| Reimplement each operation with `minio-go` | No signing work | Every operation has to be modelled; ranges, conditionals and multipart get missed easily; new backend features never reach clients |

So the adapter is a proxy. The only hard part — two signature schemes — is
pinned by the end-to-end tests.

## Supported operations

Forwarded verbatim to MinIO: `PUT/GET/HEAD/DELETE Object`, `Range GET`,
`Copy Object`, `List Objects (v1)`, `Create/Delete/HEAD Bucket`,
`Multipart Upload` (`?uploads`, `?uploadId`, `?partNumber`), `?acl`, `?tagging`,
`?versioning` and every other S3 sub-resource.

Answered by the adapter itself (OBS-only, nothing to map onto MinIO):

| Request | Behaviour |
|---|---|
| `HEAD/GET /?apiversion` | `200` + `x-obs-api: 3.0`. **Mandatory**: the Java SDK probes it before every operation and a 404 fails the whole call |
| `GET ?location` | `<LocationConstraint>` with the configured region |
| `GET ?storageinfo` | `<StorageInfo>` with `Size`/`ObjectNumber` = 0 (placeholders, enough for clients) |
| `GET ?quota` | `<Quota>` |
| `GET ?storageClass` | `STANDARD` |
| `?append/rename/truncate/modify/fileinterface/storagePolicy/az-redundancy/…` | `501 NotImplemented` in the OBS error shape |

Inbound authentication accepts both schemes the SDK can produce:

- `Authorization: OBS <AK>:<signature>` — virtual hosted addressing
  (`https://<bucket>.<endpoint>/<key>`), the default;
- `Authorization: AWS <AK>:<signature>` — path style addressing (the SDK
  switches to it automatically when the endpoint is an IP), S3 V2 signature.

Both are `base64(HMAC-SHA1(SK, StringToSign))`; the exact layout is documented in
[docs/protocol.md](docs/protocol.md).

## Quick start

```bash
go build -o bin/obs-adapter ./cmd/obs-adapter

# Credentials live in an env file outside the repository
cp configs/minio.env.example ~/.config/obs-adapter/minio.env   # chmod 600
$EDITOR ~/.config/obs-adapter/minio.env

set -a; . ~/.config/obs-adapter/minio.env; set +a
./bin/obs-adapter -listen :8080 -domain obs.local
```

Sanity check:

```bash
curl -s http://127.0.0.1:8080/__health
curl -sI http://my-bucket.obs.local:8080/?apiversion | grep x-obs-api   # expect 3.0
```

## Container image

### Pull the published image

```bash
docker pull ghcr.io/yusiwen/obs-adapter:1.0.0     # linux/amd64 + linux/arm64

docker run -d --name obs-adapter -p 8080:8080 \
  --env-file ~/.config/obs-adapter/minio.env \
  -e OBS_ADAPTER_DOMAIN=obs.internal \
  ghcr.io/yusiwen/obs-adapter:1.0.0
```

Tags: `1.0.0`, `1.0`, `latest` for a release tag, `sha-<commit>` for a manual
run. Publishing is `.github/workflows/release.yml`: `v*` tag → build/vet/test,
multi-arch buildx push to GHCR, then a GitHub release. `.github/workflows/ci.yml`
runs the same checks on `main` and on pull requests.

### Build it yourself

```bash
docker build -t obs-adapter:dev .
# cross-build for another architecture (buildx)
docker buildx build --platform linux/amd64,linux/arm64 -t obs-adapter:dev .

docker run -d --name obs-adapter -p 8080:8080 --env-file ~/.config/obs-adapter/minio.env \
  -e OBS_ADAPTER_DOMAIN=obs.internal obs-adapter:dev
```

The image is a two-stage build: `golang:1.27-alpine` → `alpine:3.22` with only
CA certificates and the static binary (`CGO_ENABLED=0`), running as UID 65532.
`VERSION` can be injected at build time (`--build-arg VERSION=1.2.3`) and is
reported by `/__health`.

### Self-contained stack (MinIO + adapter)

`docker-compose.yaml` brings up a local MinIO and the adapter wired together —
the same shape the e2e suite validates:

```bash
configs/build-minio-image.sh          # once; see the note below
docker compose up -d --build
curl -s http://127.0.0.1:8080/__health
docker compose down -v                # stop and drop the data volume
```

To use the published adapter image instead of building it:

```bash
ADAPTER_IMAGE=ghcr.io/yusiwen/obs-adapter:1.0.0 docker compose up -d --no-build
```

Override any default with the environment or a `.env` next to the compose file:
`ADAPTER_PORT` (published adapter port), `OBS_DOMAIN`, `MINIO_ROOT_USER`/
`MINIO_ROOT_PASSWORD` (which the adapter reuses as its upstream credentials),
`OBS_AK`/`OBS_SK` (inbound test credentials), `MINIO_IMAGE`, `ADAPTER_VERSION`.

The `OBS_AK`/`OBS_SK` defaults are the test values from `test/e2e/run.sh` and are
deliberately public — set your own before exposing the port beyond loopback.

**About the MinIO image.** `minio/minio` is no longer publicly pullable from
Docker Hub (and `quay.io/minio/minio` answers 401), so the compose file expects
an image named after the upstream tag that you build once from the official
release binary:

```bash
configs/build-minio-image.sh RELEASE.2025-04-22T22-12-26Z
# fetches linux-amd64/arm64 from dl.minio.org.cn (dl.min.io drops old archives),
# builds it with configs/minio.Dockerfile, tags minio/minio:<release>
```

If you already have that image in a registry of your own, point `MINIO_IMAGE` at
it and skip the script.

Notes for deployment:

- the container listens on **8080** as a non-root user, so publish
  `-p 443:8443` and set `OBS_ADAPTER_LISTEN=:8443` when fronting OBS clients that
  expect 443, or add `--cap-add NET_BIND_SERVICE` to bind 443 inside;
- mount certificates read-only and point `OBS_ADAPTER_TLS_CERT`/`_TLS_KEY` at
  them if TLS terminates in the container;
- keep credentials in an `--env-file` (mode 600), never in the image;
- `HEALTHCHECK` probes `http://127.0.0.1:8080/__health`; override it if you
  change the listen port.

## Configuration

| flag | env | default | meaning |
|---|---|---|---|
| `-listen` | `OBS_ADAPTER_LISTEN` | `:8080` | listen address |
| `-tls-cert` / `-tls-key` | `OBS_ADAPTER_TLS_CERT/KEY` | empty | enables HTTPS when both are set |
| `-domain` | `OBS_ADAPTER_DOMAIN` | empty | base domain for virtual hosted addressing, `<bucket>.<domain>` |
| `-minio` | `OBS_ADAPTER_MINIO_ENDPOINT` | `http://127.0.0.1:9000` | upstream S3 endpoint |
| `-minio-ak` / `-minio-sk` | `OBS_ADAPTER_MINIO_AK/SK` | required | upstream credentials |
| `-minio-region` | `OBS_ADAPTER_MINIO_REGION` | `us-east-1` | SigV4 region |
| `-ak` / `-sk` | `OBS_ADAPTER_AK/SK` | empty | inbound credentials; **empty means signature verification is disabled, tests only** |
| `-max-skew` | `OBS_ADAPTER_MAX_SKEW` | `15m` | tolerated `Date` skew, `0` disables |
| `-auto-create-bucket` | `OBS_ADAPTER_AUTO_CREATE_BUCKET` | `true` | create the upstream bucket before the first write |
| `-bucket-alias` | — | empty | `obsBucket=minioBucket`, repeatable |
| `-minio-path-prefix` | `OBS_ADAPTER_MINIO_PATH_PREFIX` | empty | path prefix when MinIO sits behind a gateway |
| `-log-level` | `OBS_ADAPTER_LOG_LEVEL` | `info` | `debug/info/warn/error` |

## Pointing the Java application at it

The three-argument constructor `new ObsClient(ak, sk, endpoint)` defaults to
(verified against the real SDK):

1. **HTTPS on 443** (`httpsOnly = true` when the endpoint carries no scheme);
2. **virtual hosted addressing**, `https://<bucket>.<endpoint>/<key>`;
3. a `HEAD /?apiversion` probe before every operation, using the OBS signature
   only when the answer carries `x-obs-api >= 3.0`.

So there are two ways to wire it up.

**Option A — keep HTTPS (recommended, matches the default constructor)**

```bash
./bin/obs-adapter -listen :443 \
  -tls-cert /etc/obs-adapter/server.crt -tls-key /etc/obs-adapter/server.key \
  -domain obs.internal -ak ... -sk ... -bucket-alias legacy-bucket=test ...
```

Plus two things outside the adapter:

- DNS or `/etc/hosts`: `<bucket>.obs.internal` pointing at the adapter. OBS
  bucket names never contain dots, so one `legacy-bucket.obs.internal` record is
  enough; use a wildcard `*.obs.internal` when several buckets are involved;
- a certificate the JVM trusts: import the CA with `keytool -importcert` into the
  JDK `cacerts`, or pass a truststore at startup.

Then change only `obs.endpoint` in the application configuration to
`obs.internal`; the credentials stay as they are.

**Option B — plaintext (no certificate, lab only)**

The SDK honours a scheme prefix in the endpoint, so
`obs.endpoint=http://obs.internal` serves over port 80:

```bash
./bin/obs-adapter -listen :80 -domain obs.internal ...
```

An IP endpoint such as `obs.endpoint=192.0.2.10` also works, but the SDK then
forces path style addressing and the V2 signature (both supported here), and the
URL becomes `http://192.0.2.10/<bucket>/<key>`.

A systemd unit is provided in `configs/obs-adapter.service`.

## Testing

```bash
go test ./...          # signatures, routing, header translation, proxy path (no real storage)
test/e2e/run.sh        # real OBS Java SDK -> adapter -> MinIO
```

`test/e2e` drives the unmodified `esdk-obs-java-bundle` (3.25.5, the version the
application ships; point `OBS_SDK_JAR` at it) and checks a 128 KiB and a 32 MiB
round trip by digest, Range requests, unicode and space keys, exact `prefix`
matching, `max-keys` truncation, `404 NoSuchKey`, rejected access keys and
bucket auto provisioning — in both addressing modes.

The clients resolve every bucket host to `127.0.0.1` by default; set
`E2E_RESOLVE_IP` to aim them at an adapter running elsewhere (a container on
another host, for instance):

```bash
E2E_RESOLVE_IP=192.0.2.48 java -cp "$OBS_SDK_JAR:test/e2e/out" ObsE2E http://obs.test:18081 legacy-bucket vhost
```

## Known limitations

- OBS proprietary file APIs (`?append`, `?rename`, `?truncate`, `?fileinterface`)
  are not implemented and return 501;
- `?storageinfo` and `?quota` return placeholders, not real usage;
- unknown `x-obs-*` request headers are dropped (they carry no object semantics);
- OBS presigned URLs (`?AccessKeyId=&Expires=&Signature=`) are not implemented;
- with a **plaintext** upstream, PUT/POST bodies are spooled to a temporary file
  to compute the SHA-256 (MinIO does not accept `UNSIGNED-PAYLOAD` on a non-TLS
  connection); with **HTTPS** the payload is streamed and never touches disk.
