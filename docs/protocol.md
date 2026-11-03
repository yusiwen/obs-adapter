# OBS protocol notes (measured)

Every protocol fact this adapter relies on, with where it came from. All
"measured" items can be reproduced with the commands at the end.

> Bucket names, domains and class names below are anonymised placeholders; the
> request/response fields and structure are exactly as captured.

## 1. Objects of analysis

| Object | Location | Notes |
|---|---|---|
| `esdk-obs-java-bundle-3.25.5.jar` | application `WEB-INF/lib/` | the SDK version actually deployed |
| `esdk-obs-java-bundle-3.24.12` + `-sources` | `~/.m2/repository/com/huaweicloud/` | sources used to read the implementation |
| `esdk-obs-java-3.22.3.1` + `-sources` | same | older, unused |
| business OBS wrapper jar | `WEB-INF/lib/` | class `OBSUtil` (`put`/`get`/`delete`) |

Method: `javap -p -c` over the deployed classes, the SDK sources for the
implementation, a capture server driven by the real SDK, and an independent
Python `hmac` script that recomputes each signature and prints only booleans
(never the value).

## 2. What the application actually calls (bytecode evidence)

The wrapper class has four methods and uses three SDK calls:

```
new ObsClient(ak, sk, endpoint)                      // 3-arg constructor, default ObsConfiguration
putObject(bucket, key, InputStream, ObjectMetadata)  // metadata: Content-Type / Content-Length
getObject(bucket, key).getObjectContent()            // streamed read
deleteObject(bucket, key)
```

A second module (`ObsFileStore`) additionally uses:

```
doesObjectExist(bucket, key)        // HEAD Object
getObjectMetadata(bucket, key)      // HEAD Object, reads Content-Length
putObject(PutObjectRequest)         // same operation, request-object style
```

Conclusion: in this deployment there is **no multipart, no listBuckets, no
bucket policy/ACL call**. The mandatory set for the adapter is
PUT / GET / HEAD / DELETE Object.

## 3. Captured requests (measured)

Method: the real SDK was pointed at a dump HTTP server which prints the method,
raw path, all headers and a body digest. To keep the SDK on plaintext and a high
port the endpoint was written as `http://obs.test:18081`, and the SDK's
`setCustomizedDnsImpl` resolved every host name to 127.0.0.1. Neither trick
changes the wire format, and neither exists in the real deployment.

### 3.1 Default mode: virtual hosted addressing + OBS signature

```
HEAD /?apiversion   host=legacy-bucket.obs.test:18081
  Accept-Encoding: gzip
  User-Agent: obs-sdk-java/3.24.12
(no Authorization — this is the negotiation probe)

PUT /affix%2F2026%2F11%2F03%2Ftest.xlsx   host=legacy-bucket.obs.test:18081
  Authorization: OBS <AK>:<signature>
  Content-Type: application/vnd.openxmlformats-officedocument.spreadsheetml.sheet
  Content-Length: 5
  Date: Tue, 03 Nov 2026 03:47:15 GMT

GET    /affix%2F...xlsx    Authorization: OBS <AK>:<sig>   Accept-Encoding: identity
HEAD   /affix%2F...xlsx    Authorization: OBS <AK>:<sig>
DELETE /affix%2F...xlsx    Authorization: OBS <AK>:<sig>   Content-Length: 0
```

Notes:

- the bucket is in the `Host` header, not in the path;
- `/` inside the object key is encoded as `%2F`;
- **no `x-obs-*` request header at all** for a plain put (no ACL, storage class
  or metadata), and no `Content-MD5`, no CRC64;
- `Accept-Encoding: identity` on GET, `gzip` elsewhere;
- `HEAD /?apiversion` is repeated before PUT whenever the per-bucket auth type
  cache is cold.

### 3.2 Path style mode: S3 V2 signature

When the endpoint is an IP (the regex in
`ServiceUtils.changeFromObsConfiguration`) or `setPathStyle(true)` is used, the
SDK:

1. switches to path addressing `/<bucket>/<key>`;
2. **switches the auth type from OBS to V2**: `AWS` prefix, `x-amz-*` headers;
3. **disables the api version probe** (`authTypeNegotiation = false`).

```
PUT /legacy-bucket/affix%2F2026%2F11%2F03%2Ftest.xlsx
  Authorization: AWS <AK>:<signature>
  Content-Type: application/vnd.openxmlformats-officedocument.spreadsheetml.sheet
  Content-Length: 5
  Date: Tue, 03 Nov 2026 03:46:15 GMT
```

## 4. Signature algorithm (verified byte for byte)

Two prefixes, one algorithm
(`com.obs.services.internal.utils.AbstractAuthentication`):

```
StringToSign =
    HTTP-Verb + "\n" +
    Content-MD5 + "\n" +
    Content-Type + "\n" +
    Date + "\n" +
    CanonicalizedHeaders +        // one "lowercase-name:value" line per x-obs-* (OBS) or x-amz-* (AWS) header, sorted by name
    CanonicalizedResource

signature = base64(HMAC-SHA1(SecretKey, StringToSign))
Authorization: <OBS|AWS> <AccessKey>:<signature>
```

`CanonicalizedResource`:

- virtual hosted addressing: `/<bucket><raw path>`, e.g.
  `/legacy-bucket/affix%2F2026%2F11%2F03%2Ftest.xlsx`;
- path style: `<raw path>`, e.g. `/legacy-bucket/affix%2F...`;
- the path is used **exactly as sent on the wire** (`%2F` is preserved, not
  re-encoded);
- only whitelisted sub-resources are appended (`acl`, `policy`, `versioning`,
  `location`, `uploads`, `uploadId`, `partNumber`, `delete`, `tagging`,
  `versions`, …), with **decoded** values; `max-keys`, `prefix` and `marker` are
  **not** part of the signature.

Evidence: five captured requests (one PUT, GET, HEAD, DELETE and a list) were
recomputed with an independent Python implementation and all matched; the list
request signs the resource `/bucket` without `max-keys`/`prefix`, confirming the
whitelist rule.

## 5. The api version probe (the easiest thing to get wrong)

`AbstractClient.doActionWithResult` calls `authTypeNegotiate` before **every**
operation, which sends:

```
HEAD /<bucket>/?apiversion     (unsigned)
```

Decision logic (`AbstractRequestConvertor.parseAuthTypeInResponse`):

```java
try { resp = probe(); }
catch (ServiceException e) {
    if (e.code() == 404 || e.code() <= 0 || e.code() == 408 || e.code() >= 500) throw e;  // whole call fails
    else return AuthTypeEnum.V2;                                                         // silently falls back to V2
}
return (resp.code() == 200 && resp.header("x-obs-api") >= "3.0") ? OBS : V2;
```

So **returning 404 makes the entire operation fail** (there is no fallback), and
any other 4xx except 404/408 silently downgrades the client to the AWS V2
signature. The adapter therefore always answers `200` with
`x-obs-api: 3.0`.

## 6. Transport conclusions (measured)

- With no scheme in the endpoint the SDK uses **HTTPS** by default: with
  `endpoint=obs.test` the probe URL is `https://legacy-bucket.obs.test/?apiversion`
  on port 443 (read from the exception stack trace).
- The endpoint accepts a scheme and a port (`http://obs.test:18081` is parsed as
  plaintext on 18081); setting `endpointHttpPort` alone has no effect, because
  `HTTP_PORT` comes from the endpoint string or `ObsConstraint.HTTP_PORT`.
- Bucket name and endpoint are always joined as `bucket + "." + endpoint`
  (`ServiceUtils.generateHostnameForBucket`), **even when the endpoint is an IP**,
  so a wildcard DNS record is required in virtual hosted mode.
- The deployed configuration has `obs.endpoint=<internal OBS domain>` with no
  scheme, i.e. **HTTPS 443 + `<bucket>.<internal OBS domain>`**.

## 7. Error responses

The SDK parses
`<Error><Code>…</Code><Message>…</Message><RequestId>…</RequestId><HostId>…</HostId></Error>`
and raises `ObsException` exposing `getErrorCode()`, `getErrorMessage()` and
`getResponseCode()`. Application code branches on `NoSuchKey` and
`NoSuchBucket`. MinIO's error document can be passed through unchanged (the e2e
suite asserts `404 NoSuchKey` after a delete).

## 8. Reproducing the capture

```bash
# 1) capture server: prints method/path/headers/body digest, answers like OBS
go run ./test/dump            # listens on 127.0.0.1:18081

# 2) drive the real SDK at it (virtual hosted mode, DNS override to 127.0.0.1)
javac -cp esdk-obs-java-bundle-3.25.5.jar -d out test/e2e/*.java
java -cp esdk-obs-java-bundle-3.25.5.jar:out ObsE2E http://obs.test:18081
```

To verify a signature: plug the captured `Date`, `Content-Type` and path into the
StringToSign of section 4, recompute with any HMAC-SHA1 implementation and
compare against the `Authorization` value.
