package io.queryforge;

import java.util.ArrayList;
import java.util.Collections;
import java.util.List;
import java.util.Map;

/**
 * One structured finding inside a failed response.
 *
 * <p>The fields mirror the wire format: {@link #getCode()} is the engine's own error code
 * ({@code unknown_field}, {@code value_out_of_domain}, …), {@link #getPath()} locates the node in
 * the AST, {@link #getField()} names the offending field when there is one, and
 * {@link #getSuggestions()} holds nearest-match field names for a misspelling.
 */
public final class Detail {

    private final String code;
    private final String path;
    private final String field;
    private final String message;
    private final List<String> suggestions;

    Detail(String code, String path, String field, String message, List<String> suggestions) {
        this.code = code == null ? "" : code;
        this.path = path == null ? "" : path;
        this.field = field == null ? "" : field;
        this.message = message == null ? "" : message;
        this.suggestions = suggestions == null
                ? Collections.emptyList()
                : Collections.unmodifiableList(new ArrayList<>(suggestions));
    }

    static Detail fromJson(Map<String, Object> obj) {
        return new Detail(
                Values.string(obj.get("code")),
                Values.string(obj.get("path")),
                Values.string(obj.get("field")),
                Values.string(obj.get("message")),
                Values.stringList(obj.get("suggestions")));
    }

    /** The engine's error code for this finding, e.g. {@code unknown_field}. */
    public String getCode() {
        return code;
    }

    /** Where in the AST the problem is, e.g. {@code filter.children[0]}. */
    public String getPath() {
        return path;
    }

    /** The logical field at fault, or empty when the finding is not about one field. */
    public String getField() {
        return field;
    }

    /** This finding's own message. */
    public String getMessage() {
        return message;
    }

    /** Nearest-match field names for an {@code unknown_field} finding; never null. */
    public List<String> getSuggestions() {
        return suggestions;
    }

    @Override
    public String toString() {
        return message.isEmpty() ? code : message;
    }
}
