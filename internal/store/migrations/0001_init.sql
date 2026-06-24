create extension if not exists vector with schema public;

create table readings (
  id text primary key,
  url text not null,
  url_key text not null unique,
  status text not null,
  source_kind text not null,
  title text,
  author text,
  site text,
  lang text,
  word_count int,
  extraction_mode text,
  content_key text,
  raw_key text,
  summary text,
  summary_json jsonb,
  similar_json jsonb,
  diagnostics_json jsonb,
  error text,
  process_attempts int not null default 0,
  tags text[] not null default '{}',
  search tsvector generated always as (
    to_tsvector('english',
      coalesce(title,'') || ' ' || coalesce(author,'') || ' ' ||
      coalesce(summary,'') || ' ' || array_to_string(tags,' '))
  ) stored,
  created_at timestamptz not null,
  started_at timestamptz,
  finished_at timestamptz,
  updated_at timestamptz not null
);

create index readings_search_idx on readings using gin (search);
create index readings_tags_idx on readings using gin (tags);
create index readings_page_idx on readings (created_at desc, id desc);
create index readings_status_idx on readings (status);

create table reading_vectors (
  reading_id text primary key references readings(id) on delete cascade,
  embedding vector(1536) not null
);

create index reading_vectors_ann_idx on reading_vectors using hnsw (embedding vector_cosine_ops);
