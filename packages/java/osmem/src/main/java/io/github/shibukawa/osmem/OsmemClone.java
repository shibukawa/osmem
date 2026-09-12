package io.github.shibukawa.osmem;

import java.net.URI;

/** A clone of the base cluster served on its own port. */
public final class OsmemClone implements AutoCloseable {
    private final OsmemServer server;
    private final String id;
    private final String url;

    OsmemClone(OsmemServer server, String id, String url) {
        this.server = server;
        this.id = id;
        this.url = url;
    }

    public String id() {
        return id;
    }

    /** Base URL to give to an OpenSearch client. */
    public String url() {
        return url;
    }

    public URI uri() {
        return URI.create(url);
    }

    /** Sends a JSON request to this clone. */
    public String request(String method, String path, String jsonBody) {
        return server.send(url + path, method, jsonBody);
    }

    /** Closes the clone and frees its port. */
    @Override
    public void close() {
        try {
            server.request("DELETE", "/_osmem/clones/" + id, null);
        } catch (OsmemException ignored) {
            // already gone
        }
    }
}
