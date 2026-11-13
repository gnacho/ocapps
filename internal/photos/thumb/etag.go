package thumb

// Validadores HTTP derivados de la cache key del disco (con la sal de
// generación, thumbCacheVersion): cambian exactamente cuando la miniatura
// que generaría el backend cambia, así que un 304 del navegador siempre va
// seguido de bytes actualizados tras un cambio de generación.

// ETag devuelva el validador HTTP de la miniatura: la MISMA clave que usa la
// caché en disco (versión incluida), así que cambia exactamente cuando la
// miniatura que generaría el backend cambia — un 304 del navegador siempre
// responde con los bytes actualizados tras un cambio de generación.
func ETag(href, etag string, maxSize int) string {
	return `"` + cacheKey(href, etag, maxSize) + `"`
}

// VideoETag: igual que ETag para el póster de vídeo (mismo prefijo "v" que
// usa VideoPoster en su cache key).
func VideoETag(href, etag string, maxSize int) string {
	return `"v` + cacheKey(href, etag, maxSize) + `"`
}
