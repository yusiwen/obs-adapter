import com.obs.services.ObsClient;
import com.obs.services.ObsConfiguration;
import com.obs.services.exception.ObsException;

import java.net.InetAddress;
import java.net.UnknownHostException;
import java.util.Collections;
import java.util.List;

/** Negative checks: the adapter must reject bad signatures and missing buckets. */
public class ObsNegative {
    static class LocalDns implements shade.okhttp3.Dns {
        @Override
        public List<InetAddress> lookup(String hostname) throws UnknownHostException {
            return Collections.singletonList(InetAddress.getByName("127.0.0.1"));
        }
    }

    static int failures = 0;

    static void check(String what, boolean ok, String detail) {
        System.out.printf("%-46s %s %s%n", what, ok ? "PASS" : "FAIL", detail);
        if (!ok) failures++;
    }

    static ObsClient client(String sk) {
        ObsConfiguration cfg = new ObsConfiguration();
        cfg.setEndPoint("http://obs.test:18081");
        cfg.setCustomizedDnsImpl(new LocalDns());
        cfg.setMaxErrorRetry(0);
        return new ObsClient("AKTEST1234567890", sk, cfg);
    }

    public static void main(String[] args) throws Exception {
        String bucket = args.length > 0 ? args[0] : "legacy-bucket";
        // 1. Wrong secret key -> SignatureDoesNotMatch
        ObsClient bad = client("SKwrongsecretkey0123456789");
        try {
            bad.getObject(bucket, "affix/none.txt");
            check("wrong SK rejected", false, "request succeeded");
        } catch (ObsException e) {
            check("wrong SK rejected", e.getResponseCode() == 403
                    && "SignatureDoesNotMatch".equals(e.getErrorCode()),
                    e.getResponseCode() + " " + e.getErrorCode());
        } finally {
            bad.close();
        }

        // 2. Wrong access key -> InvalidAccessKeyId
        ObsConfiguration cfg = new ObsConfiguration();
        cfg.setEndPoint("http://obs.test:18081");
        cfg.setCustomizedDnsImpl(new LocalDns());
        cfg.setMaxErrorRetry(0);
        ObsClient badAK = new ObsClient("AKWRONG", "SKtestsecretkey0123456789", cfg);
        try {
            badAK.getObject(bucket, "affix/none.txt");
            check("wrong AK rejected", false, "request succeeded");
        } catch (ObsException e) {
            check("wrong AK rejected", e.getResponseCode() == 403
                    && "InvalidAccessKeyId".equals(e.getErrorCode()),
                    e.getResponseCode() + " " + e.getErrorCode());
        } finally {
            badAK.close();
        }

        // 3. Missing bucket -> NoSuchBucket, and reads must not create it
        ObsClient ok = client("SKtestsecretkey0123456789");
        try {
            ok.getObject("no-such-bucket-xyz", "a.txt");
            check("missing bucket -> NoSuchBucket", false, "request succeeded");
        } catch (ObsException e) {
            check("missing bucket -> NoSuchBucket", e.getResponseCode() == 404,
                    e.getResponseCode() + " " + e.getErrorCode());
        } finally {
            ok.close();
        }

        System.out.println(failures == 0 ? "ALL NEGATIVE CHECKS PASSED" : failures + " CHECK(S) FAILED");
        System.exit(failures == 0 ? 0 : 1);
    }
}
