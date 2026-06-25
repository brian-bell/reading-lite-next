For a personal reading service, a single managed Postgres instance is almost always the right call. It gives you transactional metadata, full-text search, and—via the `pgvector` extension—similarity search, all behind one connection string.

The temptation is to reach for a dedicated vector database the moment you want semantic search. Resist it. Keeping the embeddings in the same database as the rows they rank means similarity lives transactionally beside its metadata, and you have one fewer service to operate, secure, and pay for.

## Why one database wins

- Backups and restores cover everything at once.
- A join can rank similar readings without a network hop.
- Scaling to zero on a hosted plan keeps the bill near nothing.

When the corpus eventually outgrows a single node, the ports in front of the store let you swap the backend without touching the pipeline. Until then, one database is the simplest thing that works.