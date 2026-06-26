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
where id = $1;

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
  title = null,
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
  updated_at = $3
where id = $1;

-- name: ReplaceReadingTags :execrows
update readings
set tags = $2, updated_at = $3
where id = $1;

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
