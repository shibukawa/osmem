package jp.shibu.osmem;

/** Thrown when osmem-server cannot be started or a request fails. */
public class OsmemException extends RuntimeException {
    public OsmemException(String message) {
        super(message);
    }

    public OsmemException(String message, Throwable cause) {
        super(message, cause);
    }
}
