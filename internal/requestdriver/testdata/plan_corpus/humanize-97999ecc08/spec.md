# Spec

## Problem

`precisedelta()` currently formats the absolute magnitude of supported durations. A negative `datetime.timedelta` or numeric-seconds value therefore produces a byte-identical string to its positive counterpart; for example, `-3661` seconds and `3661` seconds both render as `1 hour, 1 minute and 1 second`. This silently removes direction from values that may represent countdown corrections, progress deltas, or signed comparisons, making the result unsafe to display when callers need to distinguish negative from positive time.

The existing public documentation does not define negative-input behavior. `naturaldelta()` is a separate, directionless API and is not the source of this specification's output contract.

## Scope

This specification recommends a sign-preserving contract for `precisedelta()`; the permitted policy alternatives are recorded under Open questions.

- For negative `datetime.timedelta` and numeric-seconds inputs, `precisedelta()` preserves the original sign by placing exactly one ASCII `-` before the existing rendering of the absolute magnitude.
- The sign is based on the original input, before unit decomposition, suppression, formatting, localization, or rounding. A negative value that rounds to zero at its selected `minimum_unit` remains visibly negative.
- The magnitude continues to use the existing `minimum_unit`, `suppress`, `format`, unit wording, pluralization, rounding, and locale behavior. The sign is outside that magnitude rendering and is never emitted once per component.
- Positive values and zero retain their existing output, and values that currently fall back to `str(value)` or raise an existing validation/error condition retain that behavior.
- The public description of `precisedelta()` states how negative inputs are rendered, including the sign-preserving behavior for values rounded to zero.

## Non-goals

- Changing the directionless vocabulary or absolute-value behavior of `naturaldelta()`.
- Changing `naturaltime()` or adding tense such as “ago” or “from now” to `precisedelta()`.
- Redesigning unit selection, rounding, suppression, custom-format syntax, localization, or output vocabulary for non-negative values.
- Broadening the set of accepted input types or defining new behavior for invalid, non-finite, or otherwise unsupported values.

## Affected services and packages

The `humanize` Python package is affected, specifically its public `precisedelta()` time-humanization API and the documentation that describes it. No external service or service-side data is involved; sibling humanization APIs remain outside the change except for the explicit non-goal of preserving their current behavior.

## Acceptance criteria

1. `precisedelta()` given `datetime.timedelta(seconds=-3661)` returns exactly `"-1 hour, 1 minute and 1 second"`, while `precisedelta()` given `datetime.timedelta(seconds=3661)` continues to return exactly `"1 hour, 1 minute and 1 second"`.
2. For every negative `datetime.timedelta` whose absolute magnitude is representable, `precisedelta()` returns `"-"` followed by the output that `precisedelta()` produces for the equal positive magnitude when the same `minimum_unit`, `suppress`, and `format` arguments are used; this includes multi-unit values and `minimum_unit="minutes"`.
3. `precisedelta()` given a negative numeric-seconds value applies the same sign rule as a negative `datetime.timedelta`, including fractional numeric values, without producing negative signs inside individual unit components.
4. `precisedelta()` given a negative value whose magnitude rounds to zero at the requested minimum unit retains the leading sign—for example, a negative tenth of a second with `minimum_unit="minutes"` renders as `"-0 minutes"`—while an exactly zero value remains unsigned.
5. `precisedelta()` produces byte-identical output for existing positive and zero inputs under the existing unit, suppression, formatting, and locale combinations; no sign is added to those outputs.
6. `precisedelta()` preserves its existing fallback and error behavior for values that are not converted into a supported duration, including returning `"None"` for `None`, rather than applying a sign policy to those values.
7. The public documentation for `precisedelta()` explicitly states that supported negative inputs receive a leading `-`, that the rest of the output describes the absolute magnitude, and that a negative value can remain signed when rounding produces zero.

## Risks

- The current duration conversion removes the sign before formatting. If sign detection occurs after that normalization, negative and positive inputs will remain indistinguishable; the failure consequence is that the reported defect survives while appearing to support negative inputs.
- Sign may be lost when the magnitude is rounded to the minimum unit. If the implementation bases the prefix on the rounded result instead of the original input, small negative deltas will render as unsigned zero; the failure consequence is a remaining direction-loss boundary case.
- The sign must remain an invariant prefix around localized or custom-formatted magnitude text. If locale or format handling consumes or duplicates it, callers may receive inconsistent or malformed output; the failure consequence is broken UI comparisons and inconsistent output across locales.
- Numeric seconds are part of the `precisedelta()` input contract as well as `datetime.timedelta`. If only timedelta instances receive sign handling, negative numeric deltas remain silently corrupted; the failure consequence is an incomplete fix for callers using the numeric API.
- Positive output is an existing compatibility surface. If the change alters positive, zero, fallback, or existing-error behavior, downstream consumers may see unrelated regressions; the failure consequence is a broader breaking change than the negative-input defect requires.

## Open questions

None.
