# Voice Timeline Renderer

Voice Timeline Renderer is a self-hosted Go service for turning validated PCM
WAV recordings into a crossfaded, peak-normalized mixdown. The service exposes
an idempotent HTTP surface, keeps media and metadata under one data directory,
and records every render transition.

The implementation covers:

- 16-bit integer PCM WAV validation and signal measurement.
- Deterministic, client-independent waveform peak summaries for registered
  sources at any frame range and timeline target sample rate.
- Ordered timeline spans with source-frame selection, gain, and crossfade.
- Linear sample-rate conversion and mono/stereo channel mapping.
- Equal-power neighboring-span crossfades.
- Peak normalization into a requested dBFS target.
- Durable render states, receipts, and atomically committed output folders.

Automated tests are intentionally deferred. A later task stage will add unit,
integration, and end-to-end verification around the production interfaces.

## Requirements

- Go 1.23 or newer.
- A writable local directory for source media, timeline metadata, render jobs,
  output WAV files, and receipts.

No network service, database server, or third-party Go module is required.

## Build

```bash
go build ./...
```

## Run

```bash
go run ./cmd/voiceservice \
  -addr :8093 \
  -data ./var/voice-timeline
```

The health endpoint is `GET http://127.0.0.1:8093/healthz`.

Runtime settings are supplied through arguments:

- `-addr`: HTTP listen address, default `:8093`.
- `-data`: persistent data root, default `./var/voice-timeline`.

No environment variable is required. Do not put credentials in the repository;
the service does not consume any remote credential.

## HTTP interface

### Register a source

`PUT /v1/sources/{source_id}`

```json
{
  "name": "opening take",
  "wav_base64": "UklGR..."
}
```

The source ID is chosen by the caller and must be 3-64 characters using
letters, digits, underscore, or hyphen. Repeating the same ID with exactly the
same name and bytes is idempotent. Reusing it with different content returns
the `source_conflict` error.

Response:

```json
{
  "id": "opening_take_01",
  "name": "opening take",
  "digest": "b7c0...",
  "byte_count": 96044,
  "sample_rate": 48000,
  "channels": 1,
  "bits_per_sample": 16,
  "frame_count": 48000,
  "duration_ms": 1000,
  "peak_dbfs": -8.0,
  "rms_dbfs": -11.0,
  "created_at": "2026-11-09T12:00:00Z"
}
```

### Read a source waveform summary

`GET /v1/sources/{source_id}/waveform`

The aliases `/waveform-summary` and `/peaks` resolve to the same handler.

Query parameters are optional; omitted values come from the immutable source
metadata:

- `start_frame`: first source frame in the half-open range, default `0`.
- `end_frame`: exclusive source frame bound, default the source frame count.
- `sample_rate`: timeline target sample rate, default the source sample rate.
  It must be 8000-192000.
- `buckets`: number of equal-time envelope buckets, default `256` (capped by
  the range length). An explicit value must be 1-4096 and cannot exceed the
  requested source frame range.

Example:

```text
GET /v1/sources/opening_take_01/waveform?start_frame=0&end_frame=48000&sample_rate=24000&buckets=128
```

Response:

```json
{
  "schema_version": 1,
  "source_id": "opening_take_01",
  "source_digest": "b7c0...",
  "source_sample_rate": 48000,
  "source_frame_count": 48000,
  "source_peak_dbfs": -8.0,
  "start_frame": 0,
  "end_frame": 48000,
  "sample_rate": 24000,
  "output_frames": 24000,
  "bucket_count": 128,
  "silent": false,
  "min": -0.398,
  "max": 0.398,
  "peak_dbfs": -8.0,
  "buckets": [
    {
      "start_frame": 0,
      "end_frame": 375,
      "output_start_frame": 0,
      "output_end_frame": 187,
      "min": -0.284,
      "max": 0.284
    }
  ]
}
```

Each bucket reports the exact lowest and highest decoded sample amplitude in
its slice, across all channels. Values are never gained, normalized, clipped,
or resampled, so the summary cannot invent or hide source peaks. Bucket
boundaries are equal fractions of the requested time range, independent of
the target rate; `sample_rate` only determines the `output_*` labels and
`output_frames`, using the same rounding rule as timeline resampling.

Responses are deterministic and byte-stable for the same request. A compact
full-range envelope is persisted with the source at registration and reused
when its bucket boundaries divide evenly; any other request is rebuilt from
the immutable media bytes. Concurrent reads and renders therefore observe the
same values.

Failure modes remain distinct:

- An empty or malformed range returns `invalid_frame_range`.
- A range outside the source frame count returns
  `waveform_range_out_of_bounds`; it is never clamped to the source duration.
- An unreadable stored payload returns `source_decode_failed`, while a digest
  disagreement returns `source_integrity_error`.
- A genuinely silent range returns HTTP 200 with `silent: true`,
  `min: 0`, `max: 0`, and `peak_dbfs: -120`.

This endpoint does not alter source metadata, the rendered WAV download, or
the render receipt interface.

### Compose a timeline

`PUT /v1/timelines/{timeline_id}`

```json
{
  "name": "two part narration",
  "sample_rate": 48000,
  "output_channels": 2,
  "target_peak_dbfs": -1,
  "spans": [
    {
      "source_id": "opening_take_01",
      "source_start_frame": 0,
      "source_end_frame": 48000,
      "gain_db": -2,
      "crossfade_frames": 0
    },
    {
      "source_id": "closing_take_01",
      "source_start_frame": 0,
      "source_end_frame": 44100,
      "gain_db": 0,
      "crossfade_frames": 4800
    }
  ]
}
```

Spans are sequential. A later span overlaps its predecessor by
`crossfade_frames`; the overlap is mixed with equal-power weights. A crossfade
must fit within both neighboring spans.

### Start a render

`PUT /v1/renders/{render_id}`

```json
{
  "timeline_id": "two_part_narration_01"
}
```

The render runs through the durable states
`queued -> rendering -> finalizing -> completed`. A failed render records a
terminal `failed` state with a stable error code and does not expose a partial
output folder.

### Read resources

- `GET /v1/sources/{source_id}`
- `GET /v1/sources/{source_id}/waveform`
- `GET /v1/timelines/{timeline_id}`
- `GET /v1/renders/{render_id}`
- `GET /v1/renders/{render_id}/receipt`
- `GET /v1/renders/{render_id}/output`

The output route returns `audio/wav`. The receipt records source digests, span
placement, channel mapping, measured peak and RMS amplitude, normalization
gain, output digest, and non-fatal warnings.

## Data directory

```text
var/voice-timeline/
  sources/<source_id>/
    source.wav
    metadata.json
    waveform.json
  timelines/<timeline_id>.json
  jobs/<render_id>.json
  renders/<render_id>/
    output.wav
    receipt.json
  tmp/
```

Source and render folders are assembled in `tmp/` and moved into place with one
directory rename. Metadata updates use same-directory temporary files followed
by rename.

## Source layout

```text
cmd/voiceservice/       service entry point
cmd/timelinecheck/      workflow execution entry point
internal/httpapi/       routing, request limits, JSON errors, media response
internal/service/       workflow orchestration and idempotency
internal/domain/        entities, validation, errors, and render transitions
internal/audiocodec/    WAV handling, transforms, and signal measurement
internal/render/        timeline preparation, mixing, normalization, receipt
internal/storage/       atomic local repository
internal/checkflow/     bounded production workflow checks
```

## Workflow checks

Run each declared production check from the repository root:

```bash
go run ./cmd/timelinecheck -workflow ingest-audio-source
go run ./cmd/timelinecheck -workflow compose-timeline
go run ./cmd/timelinecheck -workflow render-mixdown
```

Each check creates an isolated temporary data directory, starts the real HTTP
handler, sends generated WAV inputs, and verifies read-back or rendered output.
The checks are bounded and do not require network access.

## Deferred test boundary

This baseline contains no unit tests, test fixtures, browser checks, or
`test_command`. The downstream task stage owns test creation and red/green
verification. It should extend coverage around WAV chunk edge cases, timeline
crossfade bounds, failed job cleanup, concurrent idempotency, and HTTP error
mapping.
