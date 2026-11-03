import com.obs.services.ObsClient;
import com.obs.services.ObsConfiguration;
import com.obs.services.model.GetObjectRequest;
import com.obs.services.model.ObjectMetadata;
import com.obs.services.model.ObsObject;

import java.io.ByteArrayInputStream;
import java.io.InputStream;
import java.net.InetAddress;
import java.net.UnknownHostException;
import java.security.MessageDigest;
import java.util.Arrays;
import java.util.Collections;
import java.util.List;
import java.util.Random;

/** Streaming checks: a 32 MiB body must pass through untouched, ranges must work. */
public class ObsLarge {
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
        ObsConfiguration cfg = new ObsConfiguration();
        cfg.setEndPoint("http://obs.test:18081");
        cfg.setCustomizedDnsImpl(new LocalDns());
        cfg.setMaxErrorRetry(0);
        ObsClient c = new ObsClient("AKTEST1234567890", "SKtestsecretkey0123456789", cfg);

        String bucket = args.length > 0 ? args[0] : "legacy-bucket";
        String key = "affix/2026/11/03/large-32m.bin";
        byte[] data = new byte[32 * 1024 * 1024];
        new Random(7).nextBytes(data);
        String want = sha256(data);

        long t0 = System.currentTimeMillis();
        ObjectMetadata md = new ObjectMetadata();
        md.setContentType("application/octet-stream");
        md.setContentLength((long) data.length);
        c.putObject(bucket, key, new ByteArrayInputStream(data), md);
        long putMs = System.currentTimeMillis() - t0;

        t0 = System.currentTimeMillis();
        ObsObject obj = c.getObject(bucket, key);
        byte[] got = obj.getObjectContent().readAllBytes();
        long getMs = System.currentTimeMillis() - t0;

        check("PUT/GET 32 MiB identical", want.equals(sha256(got)),
                "put " + putMs + " ms, get " + getMs + " ms, " + got.length + " bytes");

        GetObjectRequest range = new GetObjectRequest(bucket, key);
        range.setRangeStart(1024L);
        range.setRangeEnd(2047L);
        ObsObject part = c.getObject(range);
        byte[] slice = part.getObjectContent().readAllBytes();
        check("RANGE GET 1024-2047", slice.length == 1024
                        && Arrays.equals(slice, Arrays.copyOfRange(data, 1024, 2048)),
                slice.length + " bytes");

        c.deleteObject(bucket, key);
        check("DELETE large object", !c.doesObjectExist(bucket, key), "");

        System.out.println(failures == 0 ? "ALL LARGE CHECKS PASSED" : failures + " CHECK(S) FAILED");
        c.close();
        System.exit(failures == 0 ? 0 : 1);
    }

    static String sha256(byte[] b) throws Exception {
        byte[] d = MessageDigest.getInstance("SHA-256").digest(b);
        StringBuilder sb = new StringBuilder();
        for (byte x : d) sb.append(String.format("%02x", x));
        return sb.toString();
    }
}
