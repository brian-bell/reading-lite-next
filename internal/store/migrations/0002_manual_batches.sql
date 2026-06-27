create table manual_batches (
  id text primary key,
  state text not null,
  remote_id text,
  results_url text,
  processing_count int not null default 0,
  succeeded_count int not null default 0,
  errored_count int not null default 0,
  canceled_count int not null default 0,
  expired_count int not null default 0,
  created_at timestamptz not null,
  submitted_at timestamptz,
  finished_at timestamptz,
  applied_at timestamptz,
  updated_at timestamptz not null,
  constraint manual_batches_state_check check (
    state in ('planned', 'submitted', 'results_ready', 'applied', 'canceled', 'failed')
  ),
  constraint manual_batches_counts_check check (
    processing_count >= 0 and succeeded_count >= 0 and errored_count >= 0 and
    canceled_count >= 0 and expired_count >= 0
  )
);

create unique index manual_batches_remote_id_idx
  on manual_batches (remote_id)
  where remote_id is not null;

create index manual_batches_active_idx
  on manual_batches (created_at desc, id desc)
  where state in ('planned', 'submitted', 'results_ready');

create table manual_batch_items (
  custom_id text primary key,
  batch_id text not null references manual_batches(id) on delete cascade,
  reading_id text not null,
  state text not null,
  item_index int not null,
  request_json jsonb not null,
  result_json jsonb,
  error_type text,
  error_message text,
  created_at timestamptz not null,
  submitted_at timestamptz,
  finished_at timestamptz,
  applied_at timestamptz,
  updated_at timestamptz not null,
  constraint manual_batch_items_state_check check (
    state in ('planned', 'submitted', 'succeeded', 'errored', 'canceled', 'expired', 'applied', 'failed')
  )
);

create index manual_batch_items_batch_idx
  on manual_batch_items (batch_id, item_index asc);

create index manual_batch_items_reading_idx
  on manual_batch_items (reading_id);

create unique index manual_batch_items_active_reading_idx
  on manual_batch_items (reading_id)
  where state in ('planned', 'submitted', 'succeeded');
