# Ignored fields validation test

This test package indexes a keyword longer than its `ignore_above` limit.
The system test must fail with `found ignored fields in data stream`.
The false-positive harness checks that exact failure so a query that misses
ignored fields cannot silently pass.

The fixtures use Elasticsearch 8.12 and 9.3 to exercise the stored-field
and doc-value `_ignored` formats. The current stack also uses synthetic source.
