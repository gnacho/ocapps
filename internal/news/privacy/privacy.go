// Package privacy: privacy hygiene applied to feed ingestion -
// it removes URL tracking parameters and pixel trackers from the HTML.
package privacy

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// trackingPrefixes: query parameter prefixes that are dropped
// (utm_*, fbclid, gclid...). They are compared in lowercase.
var trackingPrefixes = []string{
	"utm_",        // Google Analytics / campaigns
	"fbclid",      // Facebook
	"gclid",       // Google Ads
	"gclsrc",      // Google Ads
	"msclkid",     // Microsoft Clarity
	"dclid",       // DoubleClick
	"twclid",      // Twitter/X
	"igshid",      // Instagram
	"mc_cid",      // MailChimp
	"mc_eid",      // MailChimp
	"_hsenc",      // HubSpot
	"_hsmi",       // HubSpot
	"oly_enc_id",  // Outbrain
	"oly_anon_id", // Outbrain
	"vero_id",     // Vero
	"pk_source",   // Matomo
	"pk_medium",
	"pk_campaign",
	"pk_keyword",
	"pk_content",
	"mtm_source", // Matomo Tag Manager
	"mtm_medium",
	"mtm_campaign",
	"mtm_keyword",
	"mtm_content",
	"yclid",    // Yandex
	"zanpid",   // Zanox
	"s_cid",    // Twitter
	"sfmc_id",  // Salesforce
	"wickedid", // Bing
	"mkt_tok",  // Marketo
	"cmpid",    // varios
}

// StripParams removes the tracking parameters of a URL keeping the
// rest of the query. It returns the original URL if it cannot be parsed.
func StripParams(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return raw
	}
	q := u.Query()
	if len(q) == 0 {
		return raw
	}
	changed := false
	for _, p := range trackingPrefixes {
		for k := range q {
			if strings.HasPrefix(strings.ToLower(k), p) {
				q.Del(k)
				changed = true
			}
		}
	}
	if !changed {
		return raw
	}
	u.RawQuery = q.Encode()
	return u.String()
}

var (
	imgTagRe   = regexp.MustCompile(`(?i)<img\b[^>]*>`)
	attrValRe  = regexp.MustCompile(`(?i)([a-z][a-z0-9-]*)\s*=\s*("([^"]*)"|'([^']*)')`)
	pixelSrcRe = regexp.MustCompile(`(?i)^(data:image/(gif|png|webp|jpeg);base64,[a-z0-9+/=]{0,96}|about:blank)$`)
)

// attr returns the value of the name attribute of a tag (quoted).
func attr(tag, name string) string {
	for _, m := range attrValRe.FindAllStringSubmatch(tag, -1) {
		if strings.EqualFold(m[1], name) {
			if m[3] != "" {
				return m[3]
			}
			return m[4]
		}
	}
	return ""
}

// IsTrackingPixel decides whether an <img> tag is a tracking pixel: no
// real src, transparent src or 1x1 dimensions.
func IsTrackingPixel(tag string) bool {
	lower := strings.ToLower(tag)
	src := attr(tag, "src")
	if src == "" {
		// no src and no srcset is not a real image
		return attr(tag, "srcset") == ""
	}
	if pixelSrcRe.MatchString(src) {
		return true
	}
	small := 0
	if w := attrInt(lower, "width"); w >= 0 && w <= 2 {
		small++
	}
	if h := attrInt(lower, "height"); h >= 0 && h <= 2 {
		small++
	}
	if small == 2 {
		return true
	}
	return strings.Contains(lower, "width:1px") && strings.Contains(lower, "height:1px")
}

// attrInt parses a numeric attribute, -1 if it is missing or not numeric.
func attrInt(lowerTag, name string) int {
	v := attr(lowerTag, name)
	if v == "" {
		return -1
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return -1
	}
	return n
}

// RemovePixels removes the <img> tags that are trackers and strips tracking
// parameters from the src of the remaining images. Best-effort with regex.
func RemovePixels(html string) string {
	if !strings.Contains(strings.ToLower(html), "<img") {
		return html
	}
	return imgTagRe.ReplaceAllStringFunc(html, func(tag string) string {
		if IsTrackingPixel(tag) {
			return ""
		}
		src := attr(tag, "src")
		if src == "" {
			return tag
		}
		clean := StripParams(src)
		if clean == src {
			return tag
		}
		return strings.Replace(tag, src, clean, 1)
	})
}
