# CBOM-Repository

The CBOM Repository service acts as an object storage wrapper built on top of an S3-compatible backend.
It provides a convenient REST API for uploading, retrieving, and searching Cryptographic Bills of Materials (CBOM) documents.

## Installation

Use the provided Helm chart to deploy the service into your Kubernetes cluster.
Please refer to the [Helm chart README](./deploy/charts/cbom-repository/README.md) for detailed installation instructions and configuration options.

## Status Work In Progress

This project is currently under active development.

* You can explore the current REST API design at [OpenAPI Spec](./api/openapi.yaml).
* To run the service locally or see development notes, please continue to the development guide [here](./DEV.md).

## API Endpoints

A summary of the available endpoints and methods are below. For the complete specification please see [OpenAPI Spec](./api/openapi.yaml).
Please note that HTTP API Paths have an additional default prefix `/api`. You can change it by setting the environment variable `APP_HTTP_PREFIX`.

| Path | HTTP Method | Required Params | Optional Params | Description |
|:-----|:------------|:----------------|:----------------|:------------|
| `/v1/bom`       | `POST` | Contents of BOM in request body and `Content-Type` header set | | Uploads the supplied BOM to the repository |
| `/v1/bom`       | `GET`  | query parameter `after` | query parameter `limit` | Retrieves a list of BOM serial numbers and versions created after the `after` timestamp; with `limit`, one page at a time |
| `/v1/bom/{urn}` | `GET`  | | query parameter `version` | If optional query parameter `version` is not supplied, retrieves the latest version of the BOM from repository |
| `/v1/bom/{urn}/versions` | `GET` | | | List all available versions of a BOM identified by its URN |

Let's see each endpoint in greater detail.

### POST /v1/bom (Upload)

The upload operation requires a valid `Content-Type` header. At this time, only JSON format is supported, and CycloneDX **1.6 and 1.7** are supported.
This means the request must include the header: 
```
Content-Type: application/vnd.cyclonedx+json
```

Specify the version via the media type, e.g.:
```
Content-Type: application/vnd.cyclonedx+json; version=1.7
```

When the `version` parameter is omitted, the server **auto-detects** the version from the document's own `specVersion` — so a client need not declare it.
When `version` **is** declared, the server validates against the declared version and rejects (400) a body whose `specVersion` disagrees. An unsupported version (declared or auto-detected) is also rejected with 400.

#### Upload behavior

When processing uploaded BOMs, the system recognizes several use cases:

* **BOM includes both a serial number and a version.**
  The BOM is stored exactly as provided. Subsequent attempts to upload the same serial number and version will result in a 409 Conflict response.
* **BOM includes a serial number but no version.**
  The storage layer is checked for existing BOMs with the same serial number: 
  * If matching entries are found, the uploaded BOM is assigned the next version number (latest version + 1).
  * If none exist, the uploaded BOM is stored as Version 1.
* **BOM includes neither a serial number nor a version.**
  A new URN is generated automatically. Two BOMs are stored:
  1. The original, potentially cryptographically signed, stored under the new URN with version original.
  2. A normalized version, where a serial number and version have been assigned, stored under the same URN with version 1.

Upon successful upload, the endpoint returns basic cryptographic statistics about the provided BOM.

The statistics count every component of type `cryptographic-asset` in the whole `components` tree — nested components included — by `cryptoProperties.assetType`. `metadata.component`, `metadata.tools`, `formulation` and `services` are not counted, matching the platform's asset inventory.

Each stored object carries S3 user metadata: `version` (the document version or `original`), `crypto-stats` (the statistics as JSON), `crypto-stats-version` (`2` for the tree count; absent on objects stored before nested components were counted, which hold a shallow count of the top-level array) and, only when the walk hit the 1000-level depth bound, `crypto-stats-truncated: true`. The paged search surfaces the last two as the `crypto-stats-shallow` and `crypto-stats-truncated` warnings.

This feature is still a work in progress, and both the format and the details reported may evolve over time.

### GET /v1/bom (Search)

The search operation requires a single query parameter: `after`, whose value must be a Unix timestamp (seconds).
The endpoint responds with a list of entries — one per stored document version, `original` included — created strictly after the specified timestamp.
Each entry carries `serialNumber`, `version` (a decimal integer or `original`), `created_at` (RFC 3339, second precision) and `cryptoStats`.

#### Paging with `limit`

Without `limit` the call behaves exactly as before: every matching entry, in object-store listing order; objects without statistics metadata are skipped, an object with unreadable statistics metadata fails the call, and no `warnings` field is ever emitted. Existing consumers see byte-identical responses.

With `limit` (1..1000) the call returns one page:

* entries are ordered by the store's `LastModified` at its native precision, then by object key;
* `created_at` is derived from the same listing timestamp that orders the page, so advancing `after` by it is always safe;
* the comparison with `after` is done in whole seconds, matching the precision of `created_at`;
* while more entries remain, a page holds at least `limit` entries and is then extended with every further entry sharing the last entry's second — a page never splits a second, so advancing `after` by whole seconds cannot skip anything. The page size is therefore `max(limit, entries in that second)`; `limit` bounds the `HEAD` fan-out only while seconds are sparse, and a burst of uploads stamped into one second comes back as one page (a hard limit arrives with the keyset cursor, issue #144);
* objects whose key does not follow the `<urn>-<version>` naming (only possible for objects written to the bucket outside this API) are skipped with a warning in the service log: a key with no `-` at all also fails the legacy call, while a key whose version suffix is not a positive integer or `original` is returned as-is by the legacy call;
* a page shorter than `limit` (including an empty page) is the last one;
* values above 1000 are rejected with `400` rather than clamped, so the termination rule above stays valid.

Client protocol:

```text
run_start = now()                      # your clock, or the Date header of the first response
after = watermark                      # from the previous run
loop:
    page = GET /v1/bom?after={after}&limit=N
    process page                       # deduplicate on (serialNumber, version)
    stop if len(page) < N
    after = unix(page[-1].created_at)
watermark = unix(run_start) - overlap  # overlap >= clock skew + longest upload duration + 1 s; Core uses 60 s
```

Within a run, advancing `after` by the last `created_at` is safe because a page never splits a second: every object that was listable when its page was built is yielded exactly once, and an object overwritten during the run (its `LastModified` moves) is yielded again under its new stamp — at-least-once, exactly-once for objects not modified during the run. Between runs the watermark must go back to the *start* of the previous run, not to the last `created_at`: an upload that completed while the run was already past its second — the still-open second, or a multipart upload, which S3 stamps with its **initiation** time — is only picked up by a run that starts behind it. The overlap therefore has to cover clock skew plus the longest upload you expect. Duplicates across runs are expected and deduplication is the consumer's job (Core deduplicates on `(serialNumber, version)` and already uses "job start − 60 s").

Every call lists the whole bucket — an object store cannot filter by time — so paging bounds the per-call `HEAD` fan-out and response size, not the listing cost. The change-feed decision (keyset cursor next, tracked as #144; sequence and webhook rejected for the current epic) was ratified on [#138](https://github.com/OmniTrustILM/cbom-repository/issues/138#issuecomment-5540800814).

#### Warnings (paged mode only)

With `limit`, a stored object whose statistics metadata is missing or unreadable is no longer dropped from the listing. Its entry has `"cryptoStats": null` and a `warnings` array with one of:

* `crypto-stats-missing` — the object carries no statistics metadata;
* `crypto-stats-invalid` — the statistics metadata is not valid JSON.

Two further codes can accompany valid statistics:

* `crypto-stats-shallow` — the object was stored before nested components were counted (no `crypto-stats-version` metadata), so its counts cover the top-level `components` array only;
* `crypto-stats-truncated` — the document nests deeper than 1000 levels and the counts stop there (`crypto-stats-truncated` metadata).

The document itself is retrievable as usual. The legacy call (no `limit`) and `GET /v1/bom/{urn}/versions` keep their original behaviour and never emit `warnings`; consumers get the flagged entries by adopting `limit`.

### GET /v1/bom/{urn} (Get by URN)

The get operation retrieves the latest version of a BOM—i.e., the entry with the highest version number—based on the {urn} supplied in the URL path.

The value of {urn} must conform to RFC 4122, meaning it follows the format:
```
urn:<NID>:<NSS>
```
Where:
* `<NID>` — Namespace Identifier, which must be exactly uuid for RFC 4122.
* `<NSS>` — Namespace-Specific String, which must be a valid UUID.

To retrieve a specific version instead of the latest, you may provide the optional query parameter:
```
?version=<number>
```

`version` may also be the literal `original` to fetch the unmodified upload. Any other non-empty value (`0`, `01`, `foo`) is rejected with `400`; an empty or whitespace-only value is treated as omitted (latest version).

## Full list of environment variables

The following environment variables are used to configure the `CBOM-Repository`:

| Environment Variable | Required | Default Value | Explanation |
|:---------------------|:---------|:--------------|:------------|
| `APP_LOG_LEVEL` | ![](https://img.shields.io/badge/-YES-success.svg) | `INFO` | logger level, possible values: `DEBUG`, `INFO`, `WARN`, `ERROR` |
| `APP_HTTP_PORT` | ![](https://img.shields.io/badge/-YES-success.svg) | `8080` | HTTP server port |
| `APP_HTTP_PREFIX` | ![](https://img.shields.io/badge/-YES-success.svg) | `/api` | HTTP server handlers route prefix, mainly used to mount the CBOM Repository handlers under a different starting path |
| `APP_HTTP_CORS_ALLOWED_ORIGINS` | ![](https://img.shields.io/badge/-NO-red.svg) | | comma-separated list of browser origins (`scheme://host[:port]`, no path or trailing slash) allowed to read responses cross-origin. Set it to the address of the **ILM web console**, not of this service — e.g. `https://ilm.example.net` for a console served there. Required because the console health-checks this service from the operator's browser against the `cbomRepositoryUrl` platform setting, and a browser only lets the page read the response if the page's own origin is listed. Empty disables CORS. `*` allows any origin and is intended for development only — this service has no authentication of its own, so every listed origin gets full read **and upload** access. |
| `APP_S3_ACCESS_KEY` | ![](https://img.shields.io/badge/-YES-success.svg) | | s3-compatible store access key |
| `APP_S3_SECRET_KEY` | ![](https://img.shields.io/badge/-YES-success.svg) | | s3-compatible store secret key |
| `APP_S3_REGION` | ![](https://img.shields.io/badge/-YES-success.svg) | | s3-compatible store Region |
| `APP_S3_ENDPOINT` | ![](https://img.shields.io/badge/-NO-red.svg) | | s3-compatible store endpoint, leave empty for aws roles or default aws env. variables to take precedence |
| `APP_S3_BUCKET` | ![](https://img.shields.io/badge/-YES-success.svg) | | Bucket name for the s3-compatible store. **Deployment-critical:** this value determines where BOMs are stored; changing it on an existing deployment points the service at a different bucket, so previously stored documents become inaccessible (they are not migrated). Keep it stable across upgrades. |
| `APP_S3_USE_PATH_STYLE` | ![](https://img.shields.io/badge/-YES-success.svg) | `true` | Use s3 path style |
