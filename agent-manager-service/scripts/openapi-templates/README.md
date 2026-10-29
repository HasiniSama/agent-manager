# Go client template override

`client.mustache` is the Go template exported from OpenAPI Generator **7.0.0**
(`author template -g go`), matching the version pinned in `scripts/gen_client.sh`.
The only customization is the `Message` fallback in `formatErrorMessage`, which
keeps Agent Manager's `ErrorResponse.message` in returned client error text while
preserving the upstream HTTP status and Title/Detail behavior.

Both Docker and CI JAR generation use this directory. When upgrading the
generator, re-export the upstream template and reapply that fallback. Regression
tests live in `clients/specclient/` so regeneration cannot delete them.
