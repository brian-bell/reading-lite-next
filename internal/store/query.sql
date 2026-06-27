-- name: CreateReading :one
insert into readings (
  id, url, url_key, status, source_kind, title, author, site, lang, word_count,
  extraction_mode, content_key, raw_key, summary, summary_json, similar_json,
  diagnostics_json, error, process_attempts, tags,
  created_at, started_at, finished_at, updated_at
) values (
  $1, $2, $3, $4, $5, nullif($6, ''), nullif($7, ''), nullif($8, ''), nullif($9, ''),
  $10, nullif($11, ''), nullif($12, ''), nullif($13, ''), nullif($14, ''), $15, $16,
  $17, nullif($18, ''), $19, $20,
  $21, $22, $23, $24
) on conflict (url_key) do nothing
returning id;

-- name: GetReadingByID :one
select
  id, url, url_key, status, source_kind, title, author, site, lang, word_count,
  extraction_mode, content_key, raw_key, summary, summary_json, similar_json,
  diagnostics_json, error, process_attempts, tags,
  created_at, started_at, finished_at, updated_at
from readings
where id = $1;

-- name: GetReadingByURLKey :one
select
  id, url, url_key, status, source_kind, title, author, site, lang, word_count,
  extraction_mode, content_key, raw_key, summary, summary_json, similar_json,
  diagnostics_json, error, process_attempts, tags,
  created_at, started_at, finished_at, updated_at
from readings
where url_key = $1;

-- name: UpdateReadingStatus :execrows
update readings
set
  status = $2,
  started_at = $3,
  finished_at = $4,
  error = nullif($5, ''),
  process_attempts = $6,
  updated_at = $7
where id = $1;

-- name: UpdateReadingContent :execrows
update readings
set
  title = nullif($2, ''),
  author = nullif($3, ''),
  site = nullif($4, ''),
  lang = nullif($5, ''),
  word_count = $6,
  extraction_mode = nullif($7, ''),
  content_key = nullif($8, ''),
  raw_key = nullif($9, ''),
  summary = nullif($10, ''),
  summary_json = $11,
  similar_json = $12,
  diagnostics_json = $13,
  updated_at = $14
where id = $1
  and ($15::timestamptz is null or (status = 'running' and started_at = $15));

-- name: UpdateReadingImport :execrows
update readings
set
  status = 'pending',
  source_kind = $2,
  title = nullif($3, ''),
  author = null,
  site = null,
  lang = null,
  word_count = null,
  extraction_mode = null,
  content_key = null,
  raw_key = nullif($4, ''),
  summary = null,
  summary_json = null,
  similar_json = null,
  diagnostics_json = null,
  error = null,
  process_attempts = 0,
  tags = $5,
  started_at = null,
  finished_at = null,
  updated_at = $6
where id = $1;

-- name: ReprocessReading :execrows
update readings
set
  status = 'pending',
  title = nullif($3, ''),
  author = null,
  site = null,
  lang = null,
  word_count = null,
  extraction_mode = null,
  content_key = null,
  raw_key = nullif($2, ''),
  summary = null,
  summary_json = null,
  similar_json = null,
  diagnostics_json = null,
  error = null,
  process_attempts = 0,
  started_at = null,
  finished_at = null,
  updated_at = $4
where id = $1;

-- name: ReplaceReadingTags :execrows
update readings
set tags = $2, updated_at = $3
where id = $1
  and ($4::timestamptz is null or (status = 'running' and started_at = $4));

-- name: CountReadings :one
select count(*)
from readings
where ($1 = '' or search @@ websearch_to_tsquery('english', $1))
  and (cardinality($2::text[]) = 0 or tags @> $2::text[])
  and ($3 = '' or status = $3);

-- name: SearchReadingsNewest :many
select
  id, url, url_key, status, source_kind, title, author, site, lang, word_count,
  extraction_mode, content_key, raw_key, summary, summary_json, similar_json,
  diagnostics_json, error, process_attempts, tags,
  created_at, started_at, finished_at, updated_at,
  case when $1 <> '' then ts_rank(search, websearch_to_tsquery('english', $1)) else 0::real end as search_rank
from readings
where ($1 = '' or search @@ websearch_to_tsquery('english', $1))
  and (cardinality($2::text[]) = 0 or tags @> $2::text[])
  and ($3 = '' or status = $3)
  and (
    not $4::boolean
    or case when $1 <> '' then ts_rank(search, websearch_to_tsquery('english', $1)) else 0::real end < $5::real
    or (
      case when $1 <> '' then ts_rank(search, websearch_to_tsquery('english', $1)) else 0::real end = $5::real
      and (created_at, id) < ($6::timestamptz, $7::text)
    )
  )
order by
  case when $1 <> '' then ts_rank(search, websearch_to_tsquery('english', $1)) else 0::real end desc,
  created_at desc,
  id desc
limit $8;

-- name: SearchReadingsOldest :many
select
  id, url, url_key, status, source_kind, title, author, site, lang, word_count,
  extraction_mode, content_key, raw_key, summary, summary_json, similar_json,
  diagnostics_json, error, process_attempts, tags,
  created_at, started_at, finished_at, updated_at,
  case when $1 <> '' then ts_rank(search, websearch_to_tsquery('english', $1)) else 0::real end as search_rank
from readings
where ($1 = '' or search @@ websearch_to_tsquery('english', $1))
  and (cardinality($2::text[]) = 0 or tags @> $2::text[])
  and ($3 = '' or status = $3)
  and (
    not $4::boolean
    or case when $1 <> '' then ts_rank(search, websearch_to_tsquery('english', $1)) else 0::real end < $5::real
    or (
      case when $1 <> '' then ts_rank(search, websearch_to_tsquery('english', $1)) else 0::real end = $5::real
      and (created_at, id) > ($6::timestamptz, $7::text)
    )
  )
order by
  case when $1 <> '' then ts_rank(search, websearch_to_tsquery('english', $1)) else 0::real end desc,
  created_at asc,
  id asc
limit $8;

-- name: SearchReadingsTitle :many
select
  id, url, url_key, status, source_kind, title, author, site, lang, word_count,
  extraction_mode, content_key, raw_key, summary, summary_json, similar_json,
  diagnostics_json, error, process_attempts, tags,
  created_at, started_at, finished_at, updated_at,
  case when $1 <> '' then ts_rank(search, websearch_to_tsquery('english', $1)) else 0::real end as search_rank
from readings
where ($1 = '' or search @@ websearch_to_tsquery('english', $1))
  and (cardinality($2::text[]) = 0 or tags @> $2::text[])
  and ($3 = '' or status = $3)
  and (
    not $4::boolean
    or case when $1 <> '' then ts_rank(search, websearch_to_tsquery('english', $1)) else 0::real end < $5::real
    or (
      case when $1 <> '' then ts_rank(search, websearch_to_tsquery('english', $1)) else 0::real end = $5::real
      and (lower(coalesce(title, '')), id) > (lower($6::text), $7::text)
    )
  )
order by
  case when $1 <> '' then ts_rank(search, websearch_to_tsquery('english', $1)) else 0::real end desc,
  lower(coalesce(title, '')) asc,
  id asc
limit $8;

-- name: ListNonTerminalReadings :many
select id, process_attempts
from readings
where status = 'pending'
   or (status = 'running' and started_at < $1);

-- name: DeleteReading :execrows
delete from readings
where id = $1;

-- name: CreateManualBatch :exec
insert into manual_batches (
  id, state, remote_id, results_url,
  processing_count, succeeded_count, errored_count, canceled_count, expired_count,
  created_at, submitted_at, finished_at, applied_at, updated_at
) values (
  $1, $2, nullif($3, ''), nullif($4, ''),
  $5, $6, $7, $8, $9,
  $10, $11, $12, $13, $14
);

-- name: CreateManualBatchItem :exec
insert into manual_batch_items (
  custom_id, batch_id, reading_id, state, item_index, request_json, result_json,
  error_type, error_message, created_at, submitted_at, finished_at, applied_at, updated_at
) values (
  $1, $2, $3, $4, $5, $6, $7,
  nullif($8, ''), nullif($9, ''), $10, $11, $12, $13, $14
);

-- name: GetManualBatch :one
select
  id, state, remote_id, results_url,
  processing_count, succeeded_count, errored_count, canceled_count, expired_count,
  created_at, submitted_at, finished_at, applied_at, updated_at
from manual_batches
where id = $1;

-- name: ListManualBatches :many
select
  id, state, remote_id, results_url,
  processing_count, succeeded_count, errored_count, canceled_count, expired_count,
  created_at, submitted_at, finished_at, applied_at, updated_at
from manual_batches
where ($1 = '' or state = $1)
  and (not $2::boolean or state in ('planned', 'submitted', 'results_ready'))
order by created_at desc, id desc;

-- name: ListManualBatchItems :many
select
  custom_id, batch_id, reading_id, state, item_index, request_json, result_json,
  error_type, error_message, created_at, submitted_at, finished_at, applied_at, updated_at
from manual_batch_items
where batch_id = $1
order by item_index asc;

-- name: CountActiveManualBatchItems :one
select count(*)
from manual_batch_items
where batch_id = $1
  and state in ('planned', 'submitted', 'succeeded');

-- name: GetManualBatchItemByCustomID :one
select
  custom_id, batch_id, reading_id, state, item_index, request_json, result_json,
  error_type, error_message, created_at, submitted_at, finished_at, applied_at, updated_at
from manual_batch_items
where custom_id = $1;

-- name: SubmitManualBatch :execrows
update manual_batches
set
  state = 'submitted',
  remote_id = nullif($2, ''),
  results_url = nullif($3, ''),
  processing_count = $4,
  succeeded_count = $5,
  errored_count = $6,
  canceled_count = $7,
  expired_count = $8,
  submitted_at = $9,
  updated_at = $9
where id = $1
  and state = 'planned'
  and submitted_at is null;

-- name: SubmitManualBatchItems :execrows
update manual_batch_items
set
  state = 'submitted',
  submitted_at = $2,
  updated_at = $2
where batch_id = $1
  and state = 'planned';

-- name: UpdateManualBatchState :execrows
update manual_batches
set
  state = $2,
  results_url = coalesce(nullif($3, ''), results_url),
  processing_count = $4,
  succeeded_count = $5,
  errored_count = $6,
  canceled_count = $7,
  expired_count = $8,
  finished_at = $9,
  applied_at = $10,
  updated_at = $11
where id = $1
  and state = $12;

-- name: UpdateManualBatchActiveItemsTerminal :execrows
update manual_batch_items
set
  state = $2,
  finished_at = $3,
  updated_at = $3
where batch_id = $1
  and state in ('planned', 'submitted', 'succeeded');

-- name: WriteManualBatchItemResult :execrows
update manual_batch_items
set
  state = $2,
  result_json = $3,
  error_type = nullif($4, ''),
  error_message = nullif($5, ''),
  finished_at = $6,
  updated_at = $6
where custom_id = $1
  and state = 'submitted'
  and finished_at is null;

-- name: MarkManualBatchItemApplied :execrows
update manual_batch_items
set
  state = 'applied',
  applied_at = $2,
  updated_at = $2
where custom_id = $1
  and state = 'succeeded';
