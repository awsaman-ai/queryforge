package io.queryforge;

import java.util.List;

/**
 * The AST was legal — every field, operator and value passed ordinary validation — but broke a
 * cross-field business rule declared in the config's {@code policy.requires} (e.g. passport
 * expiry filtered without a country).
 *
 * <p>Like {@link UnsupportedRequestException}, this is a well-formed answer rather than a
 * pipeline failure, and the message is written to be shown to the person who asked.
 * {@link #getPolicyError()} carries the structured specifics — which field triggered the rule,
 * and which companion field(s) would have satisfied it — on top of the plain message; it is
 * {@code null} if the engine predates {@code policyError} on the wire.
 */
public class PolicyViolationException extends QueryForgeException {

    private static final long serialVersionUID = 1L;

    private final PolicyError policyError;

    PolicyViolationException(String message, String code, List<Detail> details, PolicyError policyError) {
        super(message, code, details);
        this.policyError = policyError;
    }

    /** The structured violation detail, or {@code null} if the engine did not send one. */
    public PolicyError getPolicyError() {
        return policyError;
    }
}
