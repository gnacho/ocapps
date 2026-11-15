package thumb

// HTTP validators derived from the on-disk cache key (including the
// generation salt, thumbCacheVersion): they change exactly when the thumbnail
// the backend would generate changes, so a browser 304 is always followed by
// updated bytes after a generation change.

// ETag returns the HTTP validator of the thumbnail: the SAME key used by the
// on-disk cache (version included), so it changes exactly when the thumbnail
// the backend would generate changes - a browser 304 always responds with the
// updated bytes after a generation change.
func ETag(href, etag string, maxSize int) string {
	return `"` + cacheKey(href, etag, maxSize) + `"`
}

// VideoETag: same as ETag for the video poster (same "v" prefix used by
// VideoPoster in its cache key).
func VideoETag(href, etag string, maxSize int) string {
	return `"v` + cacheKey(href, etag, maxSize) + `"`
}
