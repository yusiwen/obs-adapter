# obs-adapter

用 Go 实现的 **OBS 服务端 API 适配层**：对外提供一个"看起来就是华为云 OBS"的 HTTP(S) 端点，内部把请求转成 S3 签名打到 MinIO 上。

```
┌──────────────────┐   OBS 协议              ┌──────────────┐   AWS SigV4      ┌─────────┐
│ esdk-obs-java    │  OBS AK:sig(HMAC-SHA1)  │ obs-adapter  │  AK:sig(SHA256)  │         │
│ (既有 Java 应用) │ ──────────────────────> │    (Go)      │ ───────────────> │  MinIO  │
│ 虚拟主机寻址     │ <────────────────────── │              │ <─────────────── │         │
└──────────────────┘   x-obs-* 响应头        └──────────────┘   S3 XML/头       └─────────┘
```

目的是：**Java 侧一行代码都不用改**（`new ObsClient(ak, sk, endpoint)` + `putObject/getObject/deleteObject` 照旧），存储从华为 OBS 迁到自建 MinIO。

## 为什么是"反向代理 + 重签名"，而不是"用 minio-go 重新实现一遍"

| 方案 | 优点 | 缺点 |
|---|---|---|
| **本实现**：按 OBS 协议收请求 → 翻译头/签名 → 原样转发给 MinIO 的 S3 端点 | Range、条件请求、分片上传、列桶、版本、多段拷贝等 S3 语义全部自动透传；代码量小；不随 MinIO 功能演进掉队 | 需要自己实现 OBS 签名校验和 SigV4 重签名 |
| 用 minio-go 逐个操作实现 | 不用管签名细节 | 每个操作都要建模一遍，分片/条件请求/range 容易漏，MinIO 加了新能力也不会自动支持 |

所以选择代理：唯一的难点（两种签名）已被 e2e 测试锁死。

## 支持的操作

照原样透传给 MinIO 的：`PUT/GET/HEAD/DELETE Object`、`Range GET`、`Copy Object`、`List Objects (v1)`、`Create/Delete Bucket`、`HEAD Bucket`、`Multipart Upload`（`?uploads`、`?uploadId`、`?partNumber`）、`?acl`、`?tagging`、`?versioning` 等所有 S3 子资源。

适配器自己实现的（OBS 独有、MinIO 没有对应物）：

| 请求 | 行为 |
|---|---|
| `HEAD/GET /?apiversion` | `200` + `x-obs-api: 3.0`。**必须实现**：Java SDK 每个操作前都探测它，返回 404 会直接让整个调用失败 |
| `GET ?location` | `<LocationConstraint>`（返回配置的 region） |
| `GET ?storageinfo` | `<StorageInfo>`（Size/ObjectNumber 填 0，仅保证客户端不报错） |
| `GET ?quota` | `<Quota>` |
| `GET ?storageClass` | `STANDARD` |
| `?append/rename/truncate/modify/fileinterface/storagePolicy/az-redundancy/…` | `501 NotImplemented`，OBS 风格错误 XML |

入站认证同时支持两种（真实 SDK 都会用到）：

- `Authorization: OBS <AK>:<sig>` —— 虚拟主机寻址（`https://<bucket>.<endpoint>/<key>`），默认模式；
- `Authorization: AWS <AK>:<sig>` —— path-style 寻址（endpoint 写成 IP 时 SDK 会自动切到它），S3 V2 签名。

两者都是 `base64(HMAC-SHA1(SK, StringToSign))`，算法细节见 [docs/protocol.md](docs/protocol.md)。

## 快速开始

```bash
go build -o bin/obs-adapter ./cmd/obs-adapter

# 凭据放环境变量文件，不要写进仓库
cp configs/minio.env.example ~/.config/obs-adapter/minio.env   # chmod 600
$EDITOR ~/.config/obs-adapter/minio.env

set -a; . ~/.config/obs-adapter/minio.env; set +a
./bin/obs-adapter -listen :8080 -domain obs.local
```

自检：

```bash
curl -s http://127.0.0.1:8080/__health
curl -sI http://my-bucket.obs.local:8080/?apiversion | grep x-obs-api   # 期望 3.0
```

## 配置

| flag | 环境变量 | 默认 | 说明 |
|---|---|---|---|
| `-listen` | `OBS_ADAPTER_LISTEN` | `:8080` | 监听地址 |
| `-tls-cert` / `-tls-key` | `OBS_ADAPTER_TLS_CERT/KEY` | 空 | 都给了才启 HTTPS |
| `-domain` | `OBS_ADAPTER_DOMAIN` | 空 | 虚拟主机寻址的基域，`<bucket>.<domain>` |
| `-minio` | `OBS_ADAPTER_MINIO_ENDPOINT` | `http://127.0.0.1:9000` | 上游 S3 端点 |
| `-minio-ak` / `-minio-sk` | `OBS_ADAPTER_MINIO_AK/SK` | 必填 | 上游凭据 |
| `-minio-region` | `OBS_ADAPTER_MINIO_REGION` | `us-east-1` | SigV4 region |
| `-ak` / `-sk` | `OBS_ADAPTER_AK/SK` | 空 | 入站凭据；**留空=不校验签名，仅测试用** |
| `-max-skew` | `OBS_ADAPTER_MAX_SKEW` | `15m` | Date 偏移容忍，0 关闭 |
| `-auto-create-bucket` | `OBS_ADAPTER_AUTO_CREATE_BUCKET` | `true` | 写入前自动建桶 |
| `-bucket-alias` | — | 空 | `obs桶名=minio桶名`，可重复 |
| `-minio-path-prefix` | `OBS_ADAPTER_MINIO_PATH_PREFIX` | 空 | 上游在网关后面时加路径前缀 |
| `-log-level` | `OBS_ADAPTER_LOG_LEVEL` | `info` | `debug/info/warn/error` |

## 部署到既有 Java 应用前面

Java 侧用的是三参构造 `new ObsClient(ak, sk, endpoint)`，它的默认行为（已实测）是：

1. **HTTPS + 443**（endpoint 不带 scheme 时 `httpsOnly=true`）；
2. **虚拟主机寻址** `https://<bucket>.<endpoint>/<key>`；
3. 每个操作前 `HEAD /?apiversion` 探测，拿到 `x-obs-api >= 3.0` 才用 OBS 签名。

因此部署只需二选一：

**方案 A：保持 HTTPS（推荐，和现在一致）**

```bash
# 适配器持证书监听 443
./bin/obs-adapter -listen :443 -tls-cert /etc/obs-adapter/server.crt -tls-key /etc/obs-adapter/server.key \
  -domain obs.internal -ak ... -sk ... -bucket-alias legacy-bucket=test ...
```

外加两件事：
- DNS/hosts：`<bucket>.obs.internal → 适配器 IP`（OBS 的桶名不含点，所以一条 `legacy-bucket.obs.internal` 记录就够；多桶就配泛解析 `*.obs.internal`）；
- 证书要让 Java 信任：把 CA 用 `keytool -importcert` 导入应用所用 JDK 的 `cacerts`，或在启动参数里加 truststore。

然后把 `WEB-INF/classes/Config/obs.properties` 的 `obs.endpoint` 改成 `obs.internal`（AK/SK 保持不变）。

**方案 B：改成 HTTP（省掉证书，仅内网/迁移期）**

SDK 认 endpoint 的 scheme 前缀，所以 `obs.endpoint=http://obs.internal` 即可走 80 端口明文：

```bash
./bin/obs-adapter -listen :80 -domain obs.internal ...
```

`obs.endpoint=192.168.2.48` 这类 IP 写法也可用，但 SDK 会强制切到 path-style + V2 签名（本适配器同样支持），且 URL 变成 `http://192.168.2.48/<bucket>/<key>`。

systemd 单元示例见 `configs/obs-adapter.service`。

## 测试

```bash
go test ./...                       # 签名/路由/头翻译/代理链路（不连真实存储）
test/e2e/run.sh                     # 真实 OBS Java SDK → 适配器 → MinIO
```

`test/e2e` 用真实 `esdk-obs-java-bundle`（3.25.5，与生产同版本，需自行用 `OBS_SDK_JAR` 指定路径），覆盖：128 KiB 与 32 MiB 往返校验、Range、中文/空格 key、prefix/max-keys、404 NoSuchKey、错误 AK/SK 被拒、自动建桶。

## 已知限制

- OBS 私有文件 API（`?append/?rename/?truncate/?fileinterface`）不实现，返回 501；
- `?storageinfo`、`?quota` 返回占位值，不反映真实用量；
- 未知的 `x-obs-*` 请求头会被丢弃（不参与语义）；
- 未实现 OBS 预签名 URL（`?AccessKeyId=&Expires=&Signature=`）；
- 上游是 **http** 时，PUT/POST 的 body 会先落临时文件算 SHA-256（MinIO 在明文连接下不接受 `UNSIGNED-PAYLOAD`）；**https** 上游是纯流式，不落盘。
