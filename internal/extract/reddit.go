package extract

// RedditGuidance is the canonical operator-facing message for a Reddit reading.
// Reddit blocks automated fetching, so a Reddit source is never fetched or
// extracted: the pipeline fails it permanently with this guidance to import the
// content another way. It lives here, with the other source special-casing, as
// the single source of truth the pipeline reuses.
const RedditGuidance = "reddit cannot be fetched automatically; export the post or comment and import it as markdown"
