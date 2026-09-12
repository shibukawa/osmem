package dev.osmem;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.nio.file.Path;

import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.extension.RegisterExtension;

class OsmemServerTest {
    static final Path SEED = Path.of("..", "..", "internal", "serve", "testdata", "seed");

    @RegisterExtension
    static OsmemExtension osmem = OsmemExtension.seed(SEED);

    @Test
    void baseIsFrozenAndCloneIsWritable(OsmemClone clone, OsmemServer server) {
        assertTrue(server.url().startsWith("http://127.0.0.1:"));
        assertEquals(java.util.List.of("other", "products"), server.indices());
        assertThrows(OsmemException.class, () -> server.request("PUT", "/products/_doc/9", "{\"name\": \"x\"}"));
        clone.request("PUT", "/products/_doc/9", "{\"name\": \"clone\"}");
        assertTrue(clone.request("GET", "/products/_count", null).contains("\"count\":3"));
        assertTrue(server.request("GET", "/products/_count", null).contains("\"count\":2"));
    }

    @Test
    void eachTestGetsAFreshClone(OsmemClone clone) {
        assertTrue(clone.request("GET", "/products/_count", null).contains("\"count\":2"));
    }
}
