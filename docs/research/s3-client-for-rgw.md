# S3 client for the deployed Ceph RGW

Research for [#12](https://github.com/yet-an-other/pub-hub/issues/12) (part of the map, #1). Researched 2026-09-27. Sources are listed [at the end](#sources), and inline references look like [Ceph-21128].

Method:

1. **Live endpoint:** anonymous, read-only `HEAD`/`GET` requests to `https://s3.bdgn.me`. No credentials were used and nothing was written.
2. **Local test double:** the official `quay.io/ceph/ceph:v19.2.3` image, whose `radosgw` binary is the same build as the deployed one. It ran rootless on 127.0.0.1 with TLS and the SQLite `dbstore` backend, so no RADOS cluster was needed [Ceph-dbstore]. A throwaway Go harness then ran every Portal operation through the current clients and logged each request's headers:
   - aws-sdk-go-v2 `service/s3` v1.113.4, with default and with `WhenRequired` checksums;
   - minio-go v7.3.0;
   - Go 1.27.1.

   Headless Chromium loaded the results as a Reader would.
3. **Primary sources:** Ceph source at the `v19.2.3` tag, Ceph trackers, PRs and release notes, the source of both SDKs, AWS API documentation, and nginx source.

This note decides the client, its settings, the multipart question and the two buckets' policies. Bucket names, the S3 endpoint the Portal uses, and provisioning belong to [Deployment on the home server: units, nginx, secrets, provisioning](https://github.com/yet-an-other/pub-hub/issues/14). `pub-hub-artifacts` and `pub-hub-metadata` below are placeholders.

## Answer in brief

- **Deployed RGW:** Ceph **19.2.3 Squid**. An anonymous `GET https://s3.bdgn.me/swift/info` returns `"version":"19.2.3"`.
- **Client:** aws-sdk-go-v2 `service/s3`, built with `s3.New(s3.Options{…})` and path-style addressing. Set **both** `RequestChecksumCalculation` and `ResponseChecksumValidation` to `WhenRequired` (snippet in §3). minio-go v7 with `DisableMultipart: true` is a working alternative.
- **Why the checksum setting is mandatory:** with SDK defaults over HTTPS, every PutObject is sent `aws-chunked` with a trailing CRC32. RGW 19.2.3 accepts the upload and stores the bytes correctly. However, it **stores `Content-Encoding: aws-chunked` on the object and serves that header to Readers** (Ceph tracker 21128, fixed in 19.2.4). On Reef ≤ 18.2.4 the same upload fails outright.
- **Multipart: not needed.** Upload each file with one PutObject, streamed from the spooled `*os.File`, up to the 100 MB cap. The single-PUT limit is 5 GiB, so there is no threshold.
- **Other operations:** ListObjectsV2 by prefix, DeleteObjects in batches and GetObject on the metadata bucket all work unchanged. DeleteObjects must be chunked to at most 1,000 keys; RGW answers `400` to 1,001.
- **Artifact bucket policy:** a single statement grants `s3:GetObject` to `"Principal": "*"` on `arn:aws:s3:::<artifact bucket>/*`. Anonymous listing, ACL and policy reads, PUT, DELETE, and GET of a missing key all get `403`.
- **Metadata bucket:** no public statement. Add a Public Access Block, keeping in mind that two of its four flags are buggy on the deployed release:
  - **`BlockPublicAcls` must stay off.** On 19.2.0–19.2.4, setting it makes RGW reject **every** PutObject, even the owner's (fixed in 19.2.5).
  - **`BlockPublicPolicy` rejects every Allow policy on all Squid releases**, public or not (fixed only in Tentacle). Set any bucket policy first, then turn it on.
- **Credentials (§6):**
  - A provisioning-only owner user creates both buckets and owns their policies.
  - The Portal's own RGW user gets `ListBucket`/`GetObject`/`PutObject`/`DeleteObject` through a bucket-policy statement, and has `--max-buckets=-1` so it cannot create buckets.
  - The Portal's keys therefore cannot change policies, remove the Public Access Block or delete buckets.
- **Upgrade:** RGW ≥ 19.2.5 fixes the `aws-chunked` and `BlockPublicAcls` bugs. The settings above are correct before and after an upgrade, so neither change has to wait for the other.

## 1. Which RGW is deployed

### How it was established

| Anonymous, read-only request | Response | What it tells us |
|---|---|---|
| `HEAD https://s3.bdgn.me/` | `200`, `server: nginx/1.28.1`, `x-amz-request-id: tx00000b718e32ba973f7b8-006ab98a2e-19909827-default` | nginx 1.28.1 fronts the gateway. The request ID is in RGW's format, and its middle field is the hex Unix time. |
| `GET /swift/info` | `200` JSON: `"swift":{"version":"19.2.3","max_file_size":5368709120,"policies":[{"default":true,"name":"default-placement"}],…}` | RGW writes `CEPH_GIT_NICE_VER` into this field [Ceph-src-swift], so the gateway runs **Ceph 19.2.3**. `max_file_size` is the 5 GiB single-PUT cap. |
| `GET /nonexistent-bucket-zz9` | `404 NoSuchBucket`, `<HostId>19909827-default-default</HostId>` | HostId has the form `<RADOS instance id>-<zone>-<zonegroup>` [Ceph-src-hostid]. The gateway is RADOS-backed, with zone `default` and zonegroup `default`. |
| `GET /admin/info` | `403 AccessDenied` (JSON) | The admin API is routed but refuses anonymous callers. |

Cross-check: the local double's image carries the label `CEPH_SHA1=c92aebb279828e9c3c1f5d24613efca272649e62`. Its `radosgw --version` prints `ceph version 19.2.3 (c92aebb…) squid (stable)`, and its `/swift/info` reports the same version.

`/swift/info` shows the exact Ceph version to anyone who can reach `s3.bdgn.me`. That is only the LAN today. Keep the S3 hostname off every public path; this matches the hostname-only tunnel from [Public exposure of pub.bdgn.me via Cloudflare Tunnel](https://github.com/yet-an-other/pub-hub/issues/13).

### Ceph changes that matter here

Each "first release" below was verified in three ways: the merge commit's ancestry against the release tags (using GitHub's compare API), the code at each tag, and the release notes [Ceph-squid-notes] [Ceph-tentacle-notes].

| Change | Ceph source | First release | In 19.2.3? |
|---|---|---|---|
| Accept `aws-chunked` bodies with trailing checksums (`STREAMING-UNSIGNED-PAYLOAD-TRAILER`). Checksums are "extracted but not validated". Before this, aws-sdk-go-v2 uploads failed with `XAmzContentSHA256Mismatch`. | PR 54856, tracker 63153; Reef backport PR 58435 [Ceph-63153] | 19.2.0; Reef 18.2.5 | yes |
| Validate and store S3 additional checksums (`x-amz-checksum-*`, the `rgw_cksum*` sources) | PR 55076, merged to main 2024-07-03 [Ceph-55076] | Tentacle 20.x. No Squid release has it: the `v19.2.6` tree has no `rgw_cksum*` files. | no |
| Stop returning `aws-chunked` in `Content-Encoding` on Get/HeadObject | PR 64440, tracker 21128; Squid PR 65219; Tentacle PR 65218 [Ceph-21128] | 19.2.4 (2026-06-01); 20.1.1 | **no, so the bug is present** |
| Stop rejecting PutObject under `BlockPublicAcls` (misuse of `canned_acl.compare()`) | PR 69481 [Ceph-69481] | 19.2.5 (2026-07-14); 20.2.3 | **no, so the bug is present** |
| Treat an Allow statement as "public" only if it has a wildcard principal. Before this, any Allow statement without `NotPrincipal` counted as public, so `BlockPublicPolicy` refused every Allow policy. | commit 019aaa4 in PR 58686, merged to main 2024-07-25 [Ceph-58686] | Tentacle 20.1.0. Not in any Squid or Reef release: the code at `v19.2.6` and `v18.2.7` still has the old check. | **no, so the bug is present** |
| Enforce `RestrictPublicBuckets` | [Ceph-tentacle-notes] ("Added support for the RestrictPublicBuckets property") | Tentacle 20.2.0 | no. The flag is stored but has no effect. |
| `read_obj_policy()` consults `s3:prefix` when choosing between 403 and 404 | PR 68652 [Ceph-squid-notes] | 19.2.6 | n/a. Readers have no `ListBucket`, so a missing key stays `403`. |

## 2. What each client sends, and what RGW 19.2.3 does with it

Every configuration ran the same script against the local double. The script:

- uploaded seven files from spooled temp files: `index.html`, a JS file, a PNG, and names with a space, `+`, `~`, parentheses, non-ASCII characters and a literal `%20`, plus a 0-byte CSS file and a 100 MiB file;
- read each one back with HEAD and GET;
- overwrote `index.html` in place with a new `Content-Type`;
- listed by prefix with a page size of 2;
- wrote, read and missed a JSON record in the metadata bucket;
- set the Artifact policy;
- replayed the Reader's anonymous requests;
- deleted everything.

### PutObject over HTTPS

| Client configuration | Request headers (non-empty body) | Stored and served by RGW 19.2.3 |
|---|---|---|
| aws-sdk-go-v2 defaults (`WhenSupported`) | `Content-Encoding: aws-chunked`, `X-Amz-Content-Sha256: STREAMING-UNSIGNED-PAYLOAD-TRAILER`, `X-Amz-Decoded-Content-Length: <n>`, `X-Amz-Trailer: x-amz-checksum-crc32` | `200`, bytes correct. **HeadObject, GetObject and the anonymous GET all return `Content-Encoding: aws-chunked`.** |
| aws-sdk-go-v2 `WhenRequired` | `X-Amz-Content-Sha256: UNSIGNED-PAYLOAD`, `Content-Type`, `Content-Length`; no checksum headers | `200`. The exact `Content-Type` comes back, with no `Content-Encoding`. |
| minio-go v7.3.0 defaults, ≤ 16 MiB | `X-Amz-Content-Sha256: UNSIGNED-PAYLOAD`; no checksum headers | Same as `WhenRequired` |
| minio-go v7.3.0 defaults, > 16 MiB | Multipart: `POST ?uploads`, 16 MiB parts (7 for 100 MiB), then `POST ?uploadId` | `200`, ETag `…-7`. See §4. |

Why the default path goes wrong: since `service/s3` v1.73.0 (2025-01-15), the SDK computes CRC32 for every operation that *supports* a checksum [aws-s3-changelog]. Over HTTPS, for a non-empty body on an operation with trailing-checksum support (PutObject, UploadPart), it switches to a trailer. It then sends the body `aws-chunked` with `STREAMING-UNSIGNED-PAYLOAD-TRAILER` [aws-go-checksum-src]. S3 "stores the resulting object without the aws-chunked value in the content-encoding header" [S3-sigv4-streaming]. RGW has "been storing this header with objects forever" [Ceph-21128]. The fix filters the header on Get/HeadObject, so after an upgrade to ≥ 19.2.4 even objects that were stored with it are served clean.

Consequences for Artifacts served straight from the bucket (ADR 0001):

- Every Artifact response would carry a content-coding it does not have.
- Headless Chromium 153 rendered such a page anyway, so the damage is quiet rather than a broken page.
- The tracker lists boto3 and aws-sdk-php among clients that fail on it [Ceph-21128].
- nginx's gzip filter skips any response that already has `Content-Encoding` [nginx-gzip-src].
- It is wrong metadata on every object.

**A plain-HTTP test would not catch this.** Over `http://`, the same default client sends `X-Amz-Checksum-Crc32` as a header with a signed payload and no `aws-chunked`. RGW 19.2.3 stored those objects cleanly in the harness. The trailer path is taken only when `req.IsHTTPS()` [aws-go-checksum-src].

### The other operations

These results are identical across all three configurations unless noted.

- **ListObjectsV2 by prefix:** returned the exact key set, including the awkward names above and the 0-byte object, over 4 pages via continuation tokens. minio-go adds `encoding-type=url&fetch-owner=true`, and RGW handles both forms.
- **GetObject on the metadata bucket:** returns the body intact. A missing key gives `404 NoSuchKey`, surfaced as `*types.NoSuchKey` by aws-sdk-go-v2 and as `minio.ToErrorResponse(err).Code == "NoSuchKey"` by minio-go.
- **DeleteObjects:**
  - Quiet mode works. A key that never existed is not reported as an error, which matches AWS ("confirms the deletion by returning the result as deleted") [S3-DeleteObjects].
  - aws-sdk-go-v2 sends `X-Amz-Checksum-Crc32` and **no `Content-MD5`**, even under `WhenRequired`, because DeleteObjects *requires* a checksum in the S3 model [aws-go-setup-context]. RGW's multi-delete never checks `Content-MD5` [Ceph-src-multidelete], and 19.2.3 accepted it.
  - A request with 1,001 keys gets `400 BadRequest` (`rgw_delete_multi_obj_max_num`, default 1000 [Ceph-rgw-opts]).
  - minio-go's `RemoveObjects` splits batches by itself and sends `Content-MD5`.
- **Overwrite in place:** new bytes and the new `Content-Type` are visible at once to authenticated and anonymous GETs.
- **PutBucketPolicy:** also a checksum-required operation, so aws-sdk-go-v2 sends a CRC32 header. RGW 19.2.3 accepts it and answers `204`.
- **Region:** RGW uses the credential scope's region only to derive the SigV4 signing key [Ceph-src-auth]. Requests signed for `default` and for `us-east-1` were both accepted.

## 3. Recommendation: aws-sdk-go-v2 with checksums pinned to `WhenRequired`

```go
import (
	"errors"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// s3.New with explicit Options, not config.LoadDefaultConfig: no AWS_* env var
// or ~/.aws file can switch checksum behaviour back on.
func newS3(endpoint, accessKey, secretKey string) *s3.Client {
	return s3.New(s3.Options{
		BaseEndpoint: aws.String(endpoint), // e.g. "https://s3.bdgn.me"; decided in #14
		Region:       "default",            // RGW zonegroup; only feeds the SigV4 key
		UsePathStyle: true,                 // /<bucket>/<key>: no wildcard DNS or cert needed
		Credentials:  credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
		// Ceph ≤ 19.2.3 stores "Content-Encoding: aws-chunked" on objects uploaded with
		// the SDK's default trailing CRC32 over TLS (tracker 21128). Keep both pinned.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
}

// Upload one spooled file as one PUT. *os.File is seekable, so SDK retries rewind it.
_, err = c.PutObject(ctx, &s3.PutObjectInput{
	Bucket: aws.String(artifactBucket), Key: aws.String(readerPath),
	Body: f, ContentLength: aws.Int64(size), ContentType: aws.String(contentType),
})

// Startup load of metadata records, and listing a Bundle's old keys.
p := s3.NewListObjectsV2Paginator(c, &s3.ListObjectsV2Input{Bucket: &bucket, Prefix: &prefix})

// Deleting leftovers: chunk keys into requests of at most 1000.
out, err := c.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: &bucket,
	Delete: &types.Delete{Objects: chunk, Quiet: aws.Bool(true)}})
// err = the whole request failed; out.Errors = per-key failures (missing keys are not errors)

// Missing metadata record.
var nsk *types.NoSuchKey
if errors.As(err, &nsk) { /* no record */ }
```

- **Do not use** `feature/s3/manager` or `feature/s3/transfermanager`. Both switch large bodies to multipart. Plain `PutObject` never does.
- **Guard test:** add one integration test that runs against RGW **over TLS**. It uploads a file and asserts that `HeadObject` returns the exact `ContentType` and a nil `ContentEncoding`. That test catches a forgotten option or a future change to SDK defaults; the SDK already changed defaults once in v1.73.0. Pin module versions and upgrade deliberately.
- **Endpoint:** HTTPS is preferable, because the body is streamed once with `UNSIGNED-PAYLOAD`. Over plain HTTP the SDK signs the payload instead, which means reading the file twice to hash it. Both work.

### minio-go as the alternative

minio-go works on 19.2.3 with its defaults:

- It sends trailing checksums only if the client is built with `Options.TrailingHeaders: true`, which defaults to false [minio-src-api].
- GetObject verification is opt-in (`GetObjectOptions.Checksum`) [minio-src-getopts].

The equivalent settings are:

```go
minio.New(host, &minio.Options{Creds: credentials.NewStaticV4(ak, sk, ""), Secure: true,
	Region: "default", BucketLookup: minio.BucketLookupPath})
// every upload:
minio.PutObjectOptions{ContentType: ct, DisableMultipart: true}
```

Without `DisableMultipart`, any file larger than 16 MiB (`minPartSize`) becomes a multipart upload [minio-src-put].

I rank minio-go second for these reasons:

- It brings 14 third-party modules; aws-sdk-go-v2's S3 client uses only AWS modules plus `smithy-go`.
- Its errors are string codes rather than types.
- Its PutObject goes multipart unless told not to.
- MinIO's server repository is archived; its last push was in 2026-04 [minio-server-archived]. The client is still maintained (v7.3.0 on 2026-08-15).

None of these is decisive. aws-sdk-go-v2's one hazard is closed by the two pinned fields plus the TLS test.

## 4. Multipart: not needed; one PUT per file

- The single-PUT limit is 5 GiB: `rgw_max_put_size` defaults to `5_G` [Ceph-rgw-opts], and the live endpoint reports `max_file_size` 5368709120. The Publish API caps a whole upload at 100 MB, so no file comes close.
- A single PUT means:
  - one request per file;
  - atomic replacement per key, which ADR 0001's overwrite-in-place relies on;
  - no incomplete multipart uploads left behind by a crash, so no `AbortIncompleteMultipartUpload` lifecycle rule is needed;
  - an ETag that is the plain MD5 of the bytes.
- A 100 MiB single PUT completed in about 0.35 s against the local double over loopback TLS. That says nothing about LAN or disk speed, only that the client side is not a bottleneck.
- **Threshold: none.** Revisit only if the per-file limit ever grows toward gigabytes.
- The local double could not validate multipart. With the experimental `dbstore` backend, every multipart object read back wrong: correct length, but the part data was misplaced. This happened with minio-go (parallel and single-threaded) and with aws-sdk-go-v2's own sequential `CreateMultipartUpload`/`UploadPart`, while every single PUT was byte-exact. This is a `dbstore` artefact and says nothing about RADOS. It is still one more reason not to depend on multipart.

## 5. Bucket policies and Public Access Block

### Artifact bucket

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "ReadersGetArtifacts",
      "Effect": "Allow",
      "Principal": "*",
      "Action": "s3:GetObject",
      "Resource": "arn:aws:s3:::pub-hub-artifacts/*"
    }
  ]
}
```

- RGW 19.2.3 accepts both `"Principal": "*"` and `{"AWS": "*"}`.
- Optionally add a Public Access Block with **only** `IgnorePublicAcls: true`, so that a stray public ACL can never grant write or list access. Verified: with a `public-read-write` bucket ACL set, an anonymous PUT still got `403` and policy reads still got `200`.
- Do not set `BlockPublicPolicy` or `RestrictPublicBuckets` on this bucket. The policy above is public by definition [S3-BPA].

### Metadata bucket

- **No public statement.** Buckets and objects are owner-only by default [S3-BPA]. The only policy statement it may carry is the Portal grant in §6.
- Public Access Block, applied **after** any bucket policy:

```json
{"BlockPublicAcls": false, "IgnorePublicAcls": true, "BlockPublicPolicy": true, "RestrictPublicBuckets": true}
```

Verified on 19.2.3:

- the Portal can still put, get, list and delete;
- a `PutBucketPolicy` with a `"Principal": "*"` statement is refused with `403`;
- a `public-read` object ACL is accepted but ignored, so the anonymous GET still gets `403`.

Two Squid limitations apply to this configuration:

- **`BlockPublicPolicy` refuses every Allow policy on Squid.** In 19.2.3, `IsPublicStatement` returns `none_of(noprinc…)` for Allow statements without a wildcard principal, which is true whenever there is no `NotPrincipal` [Ceph-src-ispublic]. So even the non-public Portal grant in §6 counts as public. Verified: re-putting that grant with the block on got `403`, while the same grant put *before* the block kept working. To change the policy later, lift `BlockPublicPolicy`, put the policy, and set it again. This was fixed only in Tentacle [Ceph-58686].
- **`RestrictPublicBuckets` has no effect before Tentacle 20.2.0** [Ceph-tentacle-notes]. Setting it now is harmless, and it starts working after an upgrade.

**Never set `BlockPublicAcls: true` on 19.2.0–19.2.4 or 20.2.0–20.2.2.** `RGWPutObj::init_processing` checks [Ceph-src-putobj]:

```cpp
(s->canned_acl.compare("public-read") || s->canned_acl.compare("public-read-write") ||
 s->canned_acl.compare("authenticated-read"))
```

`compare()` returns 0 on a match, so this condition is true for every request, including one with no ACL at all. Every PutObject into the bucket then gets `403 AccessDenied`, even from the bucket owner. This was reproduced on 19.2.3, where the same owner's PUT to a bucket without that flag got `200`. The flag can be turned on after upgrading to ≥ 19.2.5 [Ceph-69481].

### What Readers get from RGW (verified on 19.2.3; same for every client)

| Anonymous request | Artifact bucket | Metadata bucket |
|---|---|---|
| `GET` / `HEAD` of an existing key | `200` | `403 AccessDenied` (GET tested) |
| `GET` of a missing key | **`403 AccessDenied`**; nginx maps it to 404 (ADR 0001) [S3-GetObject] | `403` |
| `GET /<bucket>/`, `GET /<bucket>/?list-type=2` | `403` | `403` |
| `GET …?acl`, `GET /<bucket>/?policy` | `403` | not tested |
| `PUT`, `DELETE` of a key | `403` | not tested |

On an anonymous `200`, RGW returns:

- `Content-Type` exactly as uploaded, including `; charset=utf-8`;
- `ETag`, the quoted MD5 for single-PUT objects;
- `Last-Modified`;
- `x-amz-version-id` and `x-rgw-object-type`.

nginx should hide `x-rgw-*` as well as the `x-amz-*` that ADR 0001 already strips. nginx does not pass the upstream `Server` header by default [nginx-proxy].

## 6. Credentials and their scope

The recommended setup was verified on 19.2.3 with two RGW users:

- **`pub-hub-owner`** creates both buckets and owns their policies and Public Access Blocks. Its keys are for provisioning only and never reach the Portal.
- **`pub-hub`** (the Portal) gets object rights through a statement added to each bucket's policy:

  ```json
  {
    "Sid": "PortalReadsAndWrites",
    "Effect": "Allow",
    "Principal": {"AWS": "arn:aws:iam:::user/pub-hub"},
    "Action": ["s3:ListBucket", "s3:GetObject", "s3:PutObject", "s3:DeleteObject"],
    "Resource": ["arn:aws:s3:::pub-hub-artifacts", "arn:aws:s3:::pub-hub-artifacts/*"]
  }
  ```

  Use the same statement on the metadata bucket, with its own ARNs. Put it **before** that bucket's Public Access Block, because Squid's `BlockPublicPolicy` refuses it afterwards (§5).

  Result: the Portal's keys could put, list, get and batch-delete on both buckets. `PutBucketPolicy`, `DeletePublicAccessBlock` and `DeleteBucket` all got `403`.
- Also give `pub-hub` `radosgw-admin user modify --uid=pub-hub --max-buckets=-1`. A negative value disables bucket creation [Ceph-man-admin]; verified: CreateBucket `403`, PutObject still `200`.

One caveat: RGW makes the writer the owner of the objects it writes, so the Portal's keys can still set ACLs on those objects. `IgnorePublicAcls` on both buckets makes that harmless.

The minimal fallback is a single `pub-hub` user that owns both buckets, with `--max-buckets=-1` set after they are created. It works, but then the Portal's keys can also change the policies and remove the Public Access Block.

## 7. Caveats

- **The test double is not the production backend.** It used `dbstore` instead of RADOS. What was tested (SigV4, `aws-chunked` handling, response headers, bucket policies and Public Access Block) is RGW front-end code shared by both backends; multipart is not (§4). The version, bugs and fixes above come from the Ceph source and release notes, not only from the harness.
- **nginx was not in the loop.** In production, the Portal's requests go over HTTP/2 to nginx 1.28.1 and then to RGW. Two things for [Deployment on the home server: units, nginx, secrets, provisioning](https://github.com/yet-an-other/pub-hub/issues/14) that could not be checked read-only:
  - nginx's default `client_max_body_size` is 1m [nginx-core], so the S3 server block needs at least 100 MB plus headroom if the Portal uploads through it;
  - the `Host` header must reach RGW unchanged, or SigV4 fails.
- **Safe settings across RGW releases.** Taken from source and release notes; only 19.2.3 was run.

  | RGW | aws-sdk-go-v2 defaults over TLS | `WhenRequired` | `BlockPublicAcls: true` | `BlockPublicPolicy: true` |
  |---|---|---|---|---|
  | Reef ≤ 18.2.4 | Upload fails (`XAmzContentSHA256Mismatch`) [Ceph-63153] | works | no such check in Reef | refuses every Allow policy |
  | Reef 18.2.5–18.2.8 | Uploads, but `aws-chunked` persists (no Reef backport of the fix) | works | no such check in Reef | refuses every Allow policy |
  | Squid 19.2.0–19.2.3 (deployed) | `aws-chunked` persists and is served | works | breaks every PutObject | refuses every Allow policy |
  | Squid 19.2.4 | Header filtered on GET | works | breaks every PutObject | refuses every Allow policy |
  | Squid ≥ 19.2.5 | Filtered | works | fine | refuses every Allow policy |
  | Tentacle 20.2.0–20.2.2 | Filtered (since 20.1.1); checksums validated | works | breaks every PutObject | refuses public policies only |
  | Tentacle ≥ 20.2.3 | Filtered | works | fine | refuses public policies only |

- **Conditional writes** were not examined. The Portal serialises writes in-process (ADR 0003).

## Sources

Ceph:

- [Ceph-src-swift] `/swift/info` writes `CEPH_GIT_NICE_VER` as `version`: https://github.com/ceph/ceph/blob/v19.2.3/src/rgw/rgw_rest_swift.cc#L1989-L1990
- [Ceph-src-hostid] `RGWSI_ZoneUtils::gen_host_id()`, the `<instance>-<zone>-<zonegroup>` format: https://github.com/ceph/ceph/blob/v19.2.3/src/rgw/services/svc_zone_utils.cc#L22-L27
- [Ceph-src-putobj] Canned-ACL check under `BlockPublicAcls` in `RGWPutObj::init_processing`: https://github.com/ceph/ceph/blob/v19.2.3/src/rgw/rgw_op.cc#L3901-L3907. The fixed form (`==`) is at the same place in `v19.2.5`.
- [Ceph-src-multidelete] `RGWDeleteMultiObj_ObjStore_S3::get_params` has no `Content-MD5` check: https://github.com/ceph/ceph/blob/v19.2.3/src/rgw/rgw_rest_s3.cc#L4187-L4201. The max-key cap is at https://github.com/ceph/ceph/blob/v19.2.3/src/rgw/rgw_op.cc#L6933
- [Ceph-src-auth] The region is used only in signing-key derivation: https://github.com/ceph/ceph/blob/v19.2.3/src/rgw/rgw_auth_s3.cc#L984-L990. Unsigned `aws-chunked` with trailers is handled at https://github.com/ceph/ceph/blob/v19.2.3/src/rgw/rgw_rest_s3.cc#L5823-L5836
- [Ceph-rgw-opts] `rgw_delete_multi_obj_max_num` (1000) and `rgw_max_put_size` (`5_G`): https://github.com/ceph/ceph/blob/v19.2.3/src/common/options/rgw.yaml.in
- [Ceph-21128] Tracker 21128, "do not persist aws-chunked content-encoding": https://tracker.ceph.com/issues/21128. PRs: https://github.com/ceph/ceph/pull/64440 (main), https://github.com/ceph/ceph/pull/65219 (Squid), https://github.com/ceph/ceph/pull/65218 (Tentacle)
- [Ceph-63153] Tracker 63153, "Uploads by AWS Go SDK v2 fail with XAmzContentSHA256Mismatch when Checksum is requested": https://tracker.ceph.com/issues/63153. PRs: https://github.com/ceph/ceph/pull/54856 and the Reef backport https://github.com/ceph/ceph/pull/58435
- [Ceph-55076] "rgw: implement S3 additional checksum support": https://github.com/ceph/ceph/pull/55076
- [Ceph-69481] "rgw/s3: fix PutObject's canned_acl comparisons for BlockPublicAcls": https://github.com/ceph/ceph/pull/69481
- [Ceph-src-ispublic] `IsPublicStatement` in 19.2.3 (the `none_of(noprinc…)` branch): https://github.com/ceph/ceph/blob/v19.2.3/src/rgw/rgw_iam_policy.cc#L1900-L1922. `BlockPublicPolicy` is enforced at https://github.com/ceph/ceph/blob/v19.2.3/src/rgw/rgw_op.cc#L8003-L8008
- [Ceph-58686] Commit 019aaa4d10, "rgw: donot check for NotPrincipal in IsPublicStatement", in PR 58686 "rgw: donot allow NotPrincipal with Allow Effect" (merged to main 2024-07-25; contained in v20.1.0 and later): https://github.com/ceph/ceph/pull/58686
- [Ceph-tentacle-notes] Tentacle release notes, v20.2.0: "Added support for the ``RestrictPublicBuckets`` property of the S3 ``PublicAccessBlock`` configuration": https://docs.ceph.com/en/latest/releases/tentacle/ (source: https://github.com/ceph/ceph/blob/main/doc/releases/tentacle.rst)
- [Ceph-squid-notes] Squid release notes (dates; pr#65219 under v19.2.4; pr#69481 under v19.2.5; pr#68652 under v19.2.6): https://docs.ceph.com/en/latest/releases/squid/ (source: https://github.com/ceph/ceph/blob/main/doc/releases/squid.rst)
- [Ceph-dbstore] DBStore, a standalone RGW backend marked experimental: https://github.com/ceph/ceph/blob/v19.2.3/src/rgw/driver/dbstore/README.md. The build enables it by default: https://github.com/ceph/ceph/blob/v19.2.3/CMakeLists.txt
- [Ceph-bucketpolicy] Ceph bucket policies (Squid): https://docs.ceph.com/en/squid/radosgw/bucketpolicy/
- [Ceph-man-admin] `radosgw-admin --max-buckets` ("negative value to disable bucket creation"): https://docs.ceph.com/en/squid/man/8/radosgw-admin/ (source: https://github.com/ceph/ceph/blob/v19.2.3/doc/man/8/radosgw-admin.rst)
- Official image used for the local double: `quay.io/ceph/ceph:v19.2.3` (label `CEPH_SHA1=c92aebb279828e9c3c1f5d24613efca272649e62`)

AWS:

- [aws-s3-changelog] aws-sdk-go-v2 `service/s3` CHANGELOG. v1.73.0 (2025-01-15) made default request checksums CRC32 and configurable via `RequestChecksumCalculation`, `request_checksum_calculation` or `AWS_REQUEST_CHECKSUM_CALCULATION`. v1.74.1 enabled response validation by default. https://github.com/aws/aws-sdk-go-v2/blob/main/service/s3/CHANGELOG.md
- [aws-go-checksum-src] Trailer vs header decision (`req.IsHTTPS()`, `EnableTrailingChecksum`): https://github.com/aws/aws-sdk-go-v2/blob/main/service/internal/checksum/middleware_compute_input_checksum.go
- [aws-go-setup-context] `RequireChecksum` forces CRC32 regardless of `WhenRequired`: https://github.com/aws/aws-sdk-go-v2/blob/main/service/internal/checksum/middleware_setup_context.go
- AWS SDK for Go v2, "Data integrity protection with checksums": https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/s3-checksums.html
- `s3.Options` (`RequestChecksumCalculation`, `ResponseChecksumValidation`, `UsePathStyle`, `BaseEndpoint`): https://pkg.go.dev/github.com/aws/aws-sdk-go-v2/service/s3#Options
- [S3-sigv4-streaming] "Amazon S3 stores the resulting object without the aws-chunked value in the content-encoding header" (quoted in Ceph PR 64440): https://docs.aws.amazon.com/AmazonS3/latest/API/sigv4-streaming.html
- [S3-DeleteObjects] Up to 1,000 keys; quiet mode; missing keys confirmed as deleted: https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteObjects.html
- [S3-GetObject] A missing key returns 403 without `s3:ListBucket` and 404 with it: https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetObject.html
- [S3-BPA] Definitions of the four Block Public Access settings and of a "public" policy: https://docs.aws.amazon.com/AmazonS3/latest/userguide/access-control-block-public-access.html

minio-go:

- [minio-src-put] `PutObject` goes multipart above `minPartSize` (16 MiB) unless `DisableMultipart` is set: https://github.com/minio/minio-go/blob/v7.3.0/api-put-object.go and https://github.com/minio/minio-go/blob/v7.3.0/constants.go
- [minio-src-api] `Options.TrailingHeaders` (default false) gates automatic checksums: https://github.com/minio/minio-go/blob/v7.3.0/api.go
- [minio-src-getopts] `GetObjectOptions.Checksum` enables `x-amz-checksum-mode`: https://github.com/minio/minio-go/blob/v7.3.0/api-get-options.go
- [minio-server-archived] The MinIO server repository is archived: https://github.com/minio/minio

nginx:

- [nginx-gzip-src] The gzip header filter skips responses that already have `Content-Encoding`: https://github.com/nginx/nginx/blob/master/src/http/modules/ngx_http_gzip_filter_module.c
- [nginx-proxy] `proxy_hide_header`: by default nginx does not pass "Date", "Server", "X-Pad" and "X-Accel-…" from the upstream: https://nginx.org/en/docs/http/ngx_http_proxy_module.html#proxy_hide_header
- [nginx-core] `client_max_body_size` defaults to `1m`: https://nginx.org/en/docs/http/ngx_http_core_module.html#client_max_body_size
