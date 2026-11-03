# OBS 协议分析（实测记录）

本文记录适配器实现所依据的全部协议事实，以及每条事实的证据来源。所有"实测"项都能用文末的命令复现。

> 说明：文中桶名、域名、类名均为脱敏后的占位值，报文字段与结构保持抓包原样。

## 1. 分析对象

| 对象 | 位置 | 说明 |
|---|---|---|
| `esdk-obs-java-bundle-3.25.5.jar` | 业务应用 `WEB-INF/lib/` | 线上实际运行的 SDK |
| `esdk-obs-java-bundle-3.21.11.jar` | 同上 | 旧版本，未使用 |
| `esdk-obs-java-bundle-3.24.12(+sources)` | `~/.m2/repository/com/huaweicloud/` | 带 sources，用于读实现 |
| `esdk-obs-java-3.22.3.1(+sources)` | 同上 | 同上 |
| 业务侧 OBS 封装 jar | `WEB-INF/lib/` | 封装类 `OBSUtil`（`put/get/delete` 三个方法） |
| `osp-doc-center-1.0.0.jar` | 同上 | `ObsFileStore` |

方法：`javap -p -c` 读字节码 + sources jar 读实现 + 真实 SDK 打抓包服务器 + 用独立脚本（Python hmac）重算签名做比对。签名比对只输出布尔结果，不打印凭据。

## 2. 业务实际调用面（实测，字节码）

业务侧封装类 `OBSUtil` 只有四个方法，且只用到三个 SDK 调用：

```
new ObsClient(ak, sk, endpoint)                      // 三参构造，默认 ObsConfiguration
putObject(bucket, key, InputStream, ObjectMetadata)  // metadata: Content-Type / Content-Length
getObject(bucket, key).getObjectContent()            // 流式读
deleteObject(bucket, key)
```

另一个模块的 `ObsFileStore` 在此基础上多用到：

```
doesObjectExist(bucket, key)        // HEAD Object
getObjectMetadata(bucket, key)      // HEAD Object，取 Content-Length
putObject(PutObjectRequest)         // 同上，只是请求对象写法
```

结论：**线上没有 multipart、没有 listBuckets、没有桶级策略/ACL 调用**。适配器的必做集合是 PUT / GET / HEAD / DELETE Object。

## 3. 报文样本（实测抓包）

抓包方式：把真实 SDK 指向一个 dump HTTP 服务器，记录方法、原始路径、全部请求头、body 摘要。为了让 SDK 走明文和高位端口，endpoint 写成 `http://obs.test:18081`，并用 SDK 的 `setCustomizedDnsImpl` 把任意域名解析到 127.0.0.1（生产环境该 DNS 实现不存在，端口/方案也不变）。

### 3.1 默认模式：虚拟主机寻址 + OBS 签名

```
HEAD /?apiversion   host=legacy-bucket.obs.test:18081
  Accept-Encoding: gzip
  User-Agent: obs-sdk-java/3.24.12
（无 Authorization —— 这是探测请求）

PUT /affix%2F2026%2F11%2F03%2Ftest.xlsx   host=legacy-bucket.obs.test:18081
  Authorization: OBS <AK>:<base64 签名>
  Content-Type: application/vnd.openxmlformats-officedocument.spreadsheetml.sheet
  Content-Length: 5
  Date: Tue, 03 Nov 2026 03:47:15 GMT

GET  /affix%2F...xlsx    Authorization: OBS <AK>:<sig>   Accept-Encoding: identity
HEAD /affix%2F...xlsx    Authorization: OBS <AK>:<sig>
DELETE /affix%2F...xlsx  Authorization: OBS <AK>:<sig>   Content-Length: 0
```

要点：

- 桶名在 Host 头里，路径里没有桶；
- 对象名的 `/` 被编码成 `%2F`；
- **请求头里一个 `x-obs-*` 都没有**（简单 put 不带 ACL/存储类/元数据），也没有 Content-MD5、没有 CRC64；
- `Accept-Encoding: identity` 出现在 GET 上，`gzip` 出现在其他请求上；
- 每次 PUT 前都会重发一次 `HEAD /?apiversion`（SDK 按桶缓存 authType，缓存失效后重探）。

### 3.2 path-style 模式：S3 V2 签名

endpoint 写成 IP（`ServiceUtils.changeFromObsConfiguration` 里的 IP 正则命中）或显式 `setPathStyle(true)` 时，SDK 会：

1. 改路径寻址 `/<bucket>/<key>`；
2. **把 authType 从 OBS 切到 V2**，加 `AWS` 前缀、`x-amz-*` 头；
3. **关闭 api version 探测**（`authTypeNegotiation=false`）。

```
PUT /legacy-bucket/affix%2F2026%2F11%2F03%2Ftest.xlsx
  Authorization: AWS <AK>:<sig>
  Content-Type: application/vnd.openxmlformats-officedocument.spreadsheetml.sheet
  Content-Length: 5
  Date: Tue, 03 Nov 2026 03:46:15 GMT
```

## 4. 签名算法（逐字节验证通过）

两种前缀，同一个算法（`com.obs.services.internal.utils.AbstractAuthentication`）：

```
StringToSign =
    HTTP-Verb + "\n" +
    Content-MD5 + "\n" +
    Content-Type + "\n" +
    Date + "\n" +
    CanonicalizedHeaders +        // 每个 x-obs-*(OBS) / x-amz-*(AWS) 头一行 "小写名:值"，按名排序
    CanonicalizedResource

signature = base64(HMAC-SHA1(SecretKey, StringToSign))
Authorization: <OBS|AWS> <AccessKey>:<signature>
```

`CanonicalizedResource`：

- 虚拟主机寻址：`/<bucket><原始路径>`，例如 `/legacy-bucket/affix%2F2026%2F11%2F03%2Ftest.xlsx`；
- path-style：`<原始路径>`，例如 `/legacy-bucket/affix%2F...`；
- 路径用的是**线上发送的编码形式**（`%2F` 保留，不重新编码）；
- 追加的子资源只包含白名单里的参数（`acl`、`policy`、`versioning`、`location`、`uploads`、`uploadId`、`partNumber`、`delete`、`tagging`、`versions` …），值用**解码后**的文本；`max-keys`、`prefix`、`marker` **不参与签名**。

证据：抓到的四条请求 + 一条 list 请求，用 Python 独立实现重算，全部 `MATCH=True`；其中 list 请求的签名资源是 `/bucket`（不含 `max-keys`/`prefix`），印证白名单规则。

## 5. api version 探测（最容易踩的坑）

`AbstractClient.doActionWithResult` 在**每次操作前**调用 `authTypeNegotiate`，后者发

```
HEAD /<bucket>/?apiversion     （不签名）
```

判定逻辑（`AbstractRequestConvertor.parseAuthTypeInResponse`）：

```java
try { resp = probe(); }
catch (ServiceException e) {
    if (e.code() == 404 || e.code() <= 0 || e.code() == 408 || e.code() >= 500) throw e;  // 整单失败
    else return AuthTypeEnum.V2;                                                          // 回退 V2
}
return (resp.code() == 200 && resp.header("x-obs-api") >= "3.0") ? OBS : V2;
```

含义：**返回 404 会让整个调用报错**（不是回退）；返回任意 4xx（除 404/408）会静默回退到 AWS V2 签名。适配器因此固定返回 `200 + x-obs-api: 3.0`。

## 6. 传输层结论（实测）

- endpoint 不带 scheme 时 **默认 HTTPS**：把 endpoint 写成 `obs.test` 时 SDK 发出的 URL 是 `https://tj-...obs.test/?apiversion`（从异常堆栈里的 URL 读到），端口 443。
- endpoint 允许带 scheme 和端口：`http://obs.test:18081` 会被解析为"明文 + 18081"，`endpointHttpPort` 单独设置则不生效（HTTP_PORT 来自 endpoint 字符串或 `ObsConstraint.HTTP_PORT`）。
- 桶名与 endpoint 拼接方式固定为 `bucket + "." + endpoint`（`ServiceUtils.generateHostnameForBucket`），**即使 endpoint 是 IP 也照样拼**，所以生产环境必须有 `*.domain` 的解析记录。
- 线上配置 `obs.properties` 里 `obs.endpoint=<内网 OBS 域名>`、无 scheme → 实际是 **HTTPS 443 + `<bucket>.<内网 OBS 域名>`**。

## 7. 错误响应

SDK 解析 `<Error><Code>…</Code><Message>…</Message><RequestId>…</RequestId><HostId>…</HostId></Error>`，并把它转成 `ObsException`（`getErrorCode()` / `getErrorMessage()` / `getResponseCode()`）。业务代码按 `NoSuchKey`、`NoSuchBucket` 判断，实测 MinIO 的错误 XML 可以直接透传（e2e 里 `GET` 已删对象得到 `404 NoSuchKey`）。

## 8. 复现抓包

```bash
# 1) 抓包服务器（打印方法/路径/头/body 摘要，按 OBS 语义回包）
go run ./test/dump            # 监听 127.0.0.1:18081

# 2) 用真实 SDK 打过去（虚拟主机模式，靠自定义 DNS 解析到 127.0.0.1）
javac -cp esdk-obs-java-bundle-3.25.5.jar -d out test/e2e/*.java
java -cp esdk-obs-java-bundle-3.25.5.jar:out ObsE2E http://obs.test:18081
```

签名比对：把抓到的 `Date`、`Content-Type`、路径代入第 4 节的 StringToSign，用任意 HMAC-SHA1 实现重算后与 `Authorization` 里的值比较即可。
