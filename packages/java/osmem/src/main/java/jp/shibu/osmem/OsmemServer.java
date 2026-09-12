package jp.shibu.osmem;

import java.io.BufferedReader;
import java.io.IOException;
import java.io.InputStream;
import java.io.InputStreamReader;
import java.io.OutputStream;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.StandardCopyOption;
import java.nio.file.attribute.PosixFilePermissions;
import java.time.Duration;
import java.util.ArrayList;
import java.util.Collections;
import java.util.List;
import java.util.Locale;
import java.util.concurrent.TimeUnit;
import java.util.regex.Matcher;
import java.util.regex.Pattern;

/**
 * A running osmem-server process hosting a base cluster.
 *
 * <pre>{@code
 * try (OsmemServer server = OsmemServer.builder().seed(Path.of("testdata/seed")).freeze(true).start();
 *      OsmemClone clone = server.clone()) {
 *     // point any OpenSearch client at clone.url()
 * }
 * }</pre>
 *
 * The binary is located from the {@code osmem.server.bin} system property, the {@code OSMEM_SERVER_BIN}
 * environment variable, or the classpath resource {@code /osmem/bin/<os>-<arch>/osmem-server} shipped in the
 * {@code osmem-server-binaries} artifact with the matching classifier.
 */
public final class OsmemServer implements AutoCloseable {
    private static final Pattern URL_RE = Pattern.compile("\"url\"\\s*:\\s*\"([^\"]+)\"");
    private static final Pattern ID_RE = Pattern.compile("\"id\"\\s*:\\s*\"([^\"]+)\"");
    private static final Pattern INDICES_RE = Pattern.compile("\"indices\"\\s*:\\s*\\[([^\\]]*)\\]");

    private final Process process;
    private final String url;
    private final List<String> indices;
    private final HttpClient http = HttpClient.newHttpClient();

    private OsmemServer(Process process, String url, List<String> indices) {
        this.process = process;
        this.url = url;
        this.indices = indices;
    }

    public static Builder builder() {
        return new Builder();
    }

    /** Base URL, e.g. {@code http://127.0.0.1:51132}. */
    public String url() {
        return url;
    }

    public URI uri() {
        return URI.create(url);
    }

    /** Indices present after seeding. */
    public List<String> indices() {
        return indices;
    }

    /** Creates a clone served on its own port; the base becomes frozen. */
    public OsmemClone clone() {
        String body = request("POST", "/_osmem/clones", "{}");
        Matcher id = ID_RE.matcher(body);
        Matcher u = URL_RE.matcher(body);
        if (!id.find() || !u.find()) {
            throw new OsmemException("osmem: unexpected clone response: " + body);
        }
        return new OsmemClone(this, id.group(1), u.group(1));
    }

    /** Rejects writes to the base until a clone is used. */
    public void freeze() {
        request("POST", "/_osmem/base/freeze", "{}");
    }

    /** Sends a JSON request to the base and returns the body; throws {@link OsmemException} on error status. */
    public String request(String method, String path, String jsonBody) {
        return send(url + path, method, jsonBody);
    }

    String send(String fullUrl, String method, String jsonBody) {
        HttpRequest.Builder b = HttpRequest.newBuilder(URI.create(fullUrl))
                .header("Content-Type", "application/json")
                .timeout(Duration.ofSeconds(30));
        HttpRequest.BodyPublisher pub = jsonBody == null
                ? HttpRequest.BodyPublishers.noBody()
                : HttpRequest.BodyPublishers.ofString(jsonBody, StandardCharsets.UTF_8);
        HttpResponse<String> resp;
        try {
            resp = http.send(b.method(method, pub).build(), HttpResponse.BodyHandlers.ofString());
        } catch (IOException e) {
            throw new OsmemException("osmem: " + method + " " + fullUrl + ": " + e.getMessage(), e);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            throw new OsmemException("osmem: interrupted", e);
        }
        if (resp.statusCode() >= 400) {
            throw new OsmemException("osmem: " + method + " " + fullUrl + ": " + resp.statusCode() + " " + resp.body());
        }
        return resp.body();
    }

    /** Stops the process: closes stdin, then destroys it after a grace period. */
    @Override
    public void close() {
        if (!process.isAlive()) {
            return;
        }
        try {
            process.getOutputStream().close();
        } catch (IOException ignored) {
            // already closed
        }
        try {
            if (!process.waitFor(5, TimeUnit.SECONDS)) {
                process.destroyForcibly();
            }
        } catch (InterruptedException e) {
            process.destroyForcibly();
            Thread.currentThread().interrupt();
        }
    }

    /** Locates the osmem-server binary, extracting the bundled one when needed. */
    public static Path resolveBinary(Path explicit) throws IOException {
        if (explicit != null) {
            return explicit;
        }
        String prop = System.getProperty("osmem.server.bin");
        if (prop != null && !prop.isEmpty()) {
            return Path.of(prop);
        }
        String env = System.getenv("OSMEM_SERVER_BIN");
        if (env != null && !env.isEmpty()) {
            return Path.of(env);
        }
        String platform = platformId();
        String name = platform.startsWith("windows") ? "osmem-server.exe" : "osmem-server";
        String resource = "/osmem/bin/" + platform + "/" + name;
        try (InputStream in = OsmemServer.class.getResourceAsStream(resource)) {
            if (in == null) {
                throw new OsmemException("osmem: no bundled binary for " + platform + " (add jp.shibu.osmem:osmem-server-binaries with classifier "
                        + platform + " or set OSMEM_SERVER_BIN)");
            }
            Path dir = Files.createTempDirectory("osmem-server");
            Path bin = dir.resolve(name);
            Files.copy(in, bin, StandardCopyOption.REPLACE_EXISTING);
            if (!platform.startsWith("windows")) {
                Files.setPosixFilePermissions(bin, PosixFilePermissions.fromString("rwx------"));
            }
            bin.toFile().deleteOnExit();
            dir.toFile().deleteOnExit();
            return bin;
        }
    }

    static String platformId() {
        String os = System.getProperty("os.name", "").toLowerCase(Locale.ROOT);
        String arch = System.getProperty("os.arch", "").toLowerCase(Locale.ROOT);
        String o = os.contains("mac") || os.contains("darwin") ? "darwin" : os.contains("win") ? "windows" : "linux";
        String a = arch.contains("aarch64") || arch.contains("arm64") ? "arm64" : "amd64";
        return o + "-" + a;
    }

    /** Builder for {@link OsmemServer#start}. */
    public static final class Builder {
        private final List<Path> seeds = new ArrayList<>();
        private boolean freeze;
        private boolean japanese = true;
        private String addr;
        private Path binary;
        private Duration startupTimeout = Duration.ofSeconds(30);

        public Builder seed(Path... paths) {
            Collections.addAll(seeds, paths);
            return this;
        }

        public Builder freeze(boolean freeze) {
            this.freeze = freeze;
            return this;
        }

        public Builder japanese(boolean japanese) {
            this.japanese = japanese;
            return this;
        }

        public Builder addr(String addr) {
            this.addr = addr;
            return this;
        }

        public Builder binary(Path binary) {
            this.binary = binary;
            return this;
        }

        public Builder startupTimeout(Duration timeout) {
            this.startupTimeout = timeout;
            return this;
        }

        /** Starts the server and waits for its ready line. */
        public OsmemServer start() {
            List<String> cmd = new ArrayList<>();
            try {
                cmd.add(resolveBinary(binary).toString());
            } catch (IOException e) {
                throw new OsmemException("osmem: cannot prepare binary: " + e.getMessage(), e);
            }
            cmd.add("--parent-pid");
            cmd.add(Long.toString(ProcessHandle.current().pid()));
            for (Path s : seeds) {
                cmd.add("--seed");
                cmd.add(s.toString());
            }
            if (freeze) {
                cmd.add("--freeze");
            }
            if (!japanese) {
                cmd.add("--no-ja");
            }
            if (addr != null) {
                cmd.add("--addr");
                cmd.add(addr);
            }
            ProcessBuilder pb = new ProcessBuilder(cmd).redirectError(ProcessBuilder.Redirect.INHERIT);
            Process p;
            try {
                p = pb.start();
            } catch (IOException e) {
                throw new OsmemException("osmem: failed to start " + cmd.get(0) + ": " + e.getMessage(), e);
            }
            String ready = readReadyLine(p, startupTimeout);
            Matcher u = URL_RE.matcher(ready);
            if (!u.find()) {
                p.destroyForcibly();
                throw new OsmemException("osmem: malformed ready line: " + ready);
            }
            List<String> indices = new ArrayList<>();
            Matcher ix = INDICES_RE.matcher(ready);
            if (ix.find()) {
                for (String s : ix.group(1).split(",")) {
                    String t = s.trim().replace("\"", "");
                    if (!t.isEmpty()) {
                        indices.add(t);
                    }
                }
            }
            return new OsmemServer(p, u.group(1), Collections.unmodifiableList(indices));
        }

        private static String readReadyLine(Process p, Duration timeout) {
            String[] result = new String[1];
            Thread t = new Thread(() -> {
                try (BufferedReader r = new BufferedReader(new InputStreamReader(p.getInputStream(), StandardCharsets.UTF_8))) {
                    String line;
                    while ((line = r.readLine()) != null) {
                        if (line.contains("\"url\"")) {
                            result[0] = line;
                            return;
                        }
                    }
                } catch (IOException ignored) {
                    // process died
                }
            }, "osmem-ready-reader");
            t.setDaemon(true);
            t.start();
            try {
                t.join(timeout.toMillis());
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
            }
            if (result[0] == null) {
                p.destroyForcibly();
                throw new OsmemException("osmem: server did not start within " + timeout);
            }
            return result[0];
        }
    }

    /** Silences unused warnings for the stdin stream we keep open on purpose. */
    @SuppressWarnings("unused")
    private OutputStream stdin() {
        return process.getOutputStream();
    }
}
