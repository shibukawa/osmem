package io.github.shibukawa.osmem;

import java.nio.file.Path;
import java.util.function.Consumer;

import org.junit.jupiter.api.extension.AfterAllCallback;
import org.junit.jupiter.api.extension.AfterEachCallback;
import org.junit.jupiter.api.extension.BeforeAllCallback;
import org.junit.jupiter.api.extension.BeforeEachCallback;
import org.junit.jupiter.api.extension.ExtensionContext;
import org.junit.jupiter.api.extension.ParameterContext;
import org.junit.jupiter.api.extension.ParameterResolver;

/**
 * JUnit 5 extension: one server per test class, one clone per test method.
 *
 * <pre>{@code
 * @RegisterExtension
 * static OsmemExtension osmem = OsmemExtension.builder(b -> b.seed(Path.of("src/test/resources/seed")).freeze(true));
 *
 * @Test
 * void search(OsmemClone clone) {
 *     OpenSearchClient client = clientFor(clone.url());
 * }
 * }</pre>
 *
 * Test methods may declare an {@link OsmemClone} or {@link OsmemServer} parameter; {@link #clone()} and
 * {@link #server()} return the same objects for the current test.
 */
public final class OsmemExtension
        implements BeforeAllCallback, AfterAllCallback, BeforeEachCallback, AfterEachCallback, ParameterResolver {
    private final Consumer<OsmemServer.Builder> configure;
    private OsmemServer server;
    private OsmemClone clone;

    private OsmemExtension(Consumer<OsmemServer.Builder> configure) {
        this.configure = configure;
    }

    public static OsmemExtension builder(Consumer<OsmemServer.Builder> configure) {
        return new OsmemExtension(configure);
    }

    public static OsmemExtension seed(Path... seeds) {
        return new OsmemExtension(b -> b.seed(seeds).freeze(true));
    }

    public OsmemServer server() {
        return server;
    }

    /** The clone of the currently running test. */
    public OsmemClone clone() {
        return clone;
    }

    @Override
    public void beforeAll(ExtensionContext context) {
        OsmemServer.Builder b = OsmemServer.builder();
        configure.accept(b);
        server = b.start();
    }

    @Override
    public void afterAll(ExtensionContext context) {
        if (server != null) {
            server.close();
            server = null;
        }
    }

    @Override
    public void beforeEach(ExtensionContext context) {
        clone = server.clone();
    }

    @Override
    public void afterEach(ExtensionContext context) {
        if (clone != null) {
            clone.close();
            clone = null;
        }
    }

    @Override
    public boolean supportsParameter(ParameterContext parameterContext, ExtensionContext extensionContext) {
        Class<?> type = parameterContext.getParameter().getType();
        return type == OsmemClone.class || type == OsmemServer.class;
    }

    @Override
    public Object resolveParameter(ParameterContext parameterContext, ExtensionContext extensionContext) {
        return parameterContext.getParameter().getType() == OsmemClone.class ? clone : server;
    }
}
