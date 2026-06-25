[Forum](/) [New](/new) [Log in](/login)

pgfan For a personal database the cheapest reliable backup is a nightly logical dump shipped to object storage. It is boring, it restores anywhere, and you can test it with a one-line restore into a throwaway container.

opsbear Logical dumps are fine until the database is large. Past a few gigabytes you want physical base backups plus WAL archiving so you get point-in-time recovery instead of losing a whole day.

pgfan Agreed, but for a reading service the corpus is tiny. Start with the dump, add WAL archiving only once a lost day actually hurts. Premature operational complexity is its own kind of outage.

#### Related threads

- [pgvector index tuning](/t/41)
- [connection pooling on Neon](/t/40)