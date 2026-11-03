import com.obs.services.ObsClient;
import com.obs.services.ObsConfiguration;
import com.obs.services.exception.ObsException;
import com.obs.services.model.*;

import java.net.InetAddress;
import java.net.UnknownHostException;
import java.security.MessageDigest;
import java.util.Collections;
import java.util.List;
import java.util.Random;

/**
 * End to end check: the real OBS Java SDK (virtual hosted style, OBS signature,
 * api version negotiation) against the Go adapter, which stores into MinIO.
 */
public class ObsE2E {
    static class LocalDns implements shade.okhttp3.Dns {
        @Override
        public List<InetAddress> lookup(String hostname) throws UnknownHostException {
            return Collections.singletonList(InetAddress.getByName(
                    System.getenv().getOrDefault("E2E_RESOLVE_IP", "127.0.0.1")));
        }
    }

    static int failures = 0;

    static void check(String what, boolean ok, String detail) {
        System.out.printf("%-46s %s %s%n", what, ok ? "PASS" : "FAIL", detail);
        if (!ok) failures++;
    }

    public static void main(String[] args) throws Exception {
        String endpoint = args.length > 0 ? args[0] : "http://obs.test:18081";
        String bucket = args.length > 1 ? args[1] : "legacy-bucket";
        String mode = args.length > 2 ? args[2] : "vhost";

        ObsConfiguration cfg = new ObsConfiguration();
        if (mode.equals("path")) {
            // Path style also switches the SDK to the S3 V2 signature (AWS prefix).
            String ip = System.getenv().getOrDefault("E2E_RESOLVE_IP", "127.0.0.1");
            String port = endpoint.substring(endpoint.lastIndexOf(':') + 1);
            cfg.setEndPoint("http://" + ip + ":" + port);
            cfg.setPathStyle(true);
        } else {
            cfg.setEndPoint(endpoint);
            cfg.setCustomizedDnsImpl(new LocalDns());
        }
        cfg.setMaxErrorRetry(0);
        ObsClient c = new ObsClient("AKTEST1234567890", "SKtestsecretkey0123456789", cfg);

        byte[] data = new byte[128 * 1024];
        new Random(42).nextBytes(data);
        String key = "affix/2026/11/03/e2e-binary.xlsx";

        try {
            ObjectMetadata md = new ObjectMetadata();
            md.setContentType("application/vnd.openxmlformats-officedocument.spreadsheetml.sheet");
            md.setContentLength((long) data.length);
            PutObjectResult put = c.putObject(bucket, key, new java.io.ByteArrayInputStream(data), md);
            check("PUT object (128 KiB)", put != null, "etag=" + put.getEtag());

            ObsObject obj = c.getObject(bucket, key);
            byte[] got = obj.getObjectContent().readAllBytes();
            check("GET object round trip", MessageDigest.isEqual(data, got),
                    got.length + " bytes, content-type=" + obj.getMetadata().getContentType());

            check("HEAD object exists", c.doesObjectExist(bucket, key), "");
            ObjectMetadata meta = c.getObjectMetadata(bucket, key);
            check("GET metadata length", meta.getContentLength() == data.length, "len=" + meta.getContentLength());

            String odd = "affix/测试 目录/文件 名 (1).xlsx";
            c.putObject(bucket, odd, new java.io.ByteArrayInputStream("中文 ok".getBytes("UTF-8")),
                    new ObjectMetadata());
            byte[] oddBack = c.getObject(bucket, odd).getObjectContent().readAllBytes();
            check("GET object with unicode + space key", new String(oddBack, "UTF-8").equals("中文 ok"),
                    new String(oddBack, "UTF-8"));

            ListObjectsRequest lr = new ListObjectsRequest(bucket);
            lr.setPrefix("affix/");
            lr.setMaxKeys(100);
            ObjectListing listing = c.listObjects(lr);
            check("LIST objects prefix=affix/", listing.getObjects().size() >= 2,
                    listing.getObjects().size() + " objects");

            ListObjectsRequest narrow = new ListObjectsRequest(bucket);
            narrow.setPrefix("affix/2026/11/03/e2e-binary");
            narrow.setMaxKeys(100);
            check("LIST honours prefix", c.listObjects(narrow).getObjects().size() == 1,
                    c.listObjects(narrow).getObjects().size() + " objects");

            ListObjectsRequest page = new ListObjectsRequest(bucket);
            page.setMaxKeys(1);
            ObjectListing one = c.listObjects(page);
            check("LIST honours max-keys", one.getObjects().size() == 1 && one.isTruncated(),
                    one.getObjects().size() + " objects, truncated=" + one.isTruncated());

            c.deleteObject(bucket, odd);
            check("DELETE object", !c.doesObjectExist(bucket, odd), "");
            c.deleteObject(bucket, key);
            check("DELETE object (binary)", !c.doesObjectExist(bucket, key), "");

            boolean notFound = false;
            try {
                c.getObject(bucket, key).getObjectContent().readAllBytes();
            } catch (ObsException e) {
                notFound = e.getResponseCode() == 404 && "NoSuchKey".equals(e.getErrorCode());
                check("GET missing object -> NoSuchKey", notFound,
                        e.getResponseCode() + " " + e.getErrorCode());
            }
            if (!notFound) check("GET missing object -> NoSuchKey", false, "no exception");

            // A bucket that was never provisioned: the adapter auto creates it.
            String created = "e2e-auto-created";
            c.putObject(created, "hello.txt", new java.io.ByteArrayInputStream("x".getBytes("UTF-8")),
                    new ObjectMetadata());
            check("PUT into non-existing bucket (auto create)", c.doesObjectExist(created, "hello.txt"), created);
            c.deleteObject(created, "hello.txt");
        } catch (Exception e) {
            check("UNEXPECTED EXCEPTION", false, e.toString());
            e.printStackTrace(System.out);
        } finally {
            c.close();
        }

        System.out.println(failures == 0 ? "ALL CHECKS PASSED" : failures + " CHECK(S) FAILED");
        System.exit(failures == 0 ? 0 : 1);
    }
}
