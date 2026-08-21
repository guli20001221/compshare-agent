package deployment

import "strings"

// image_taxonomy.go holds the platform's OWN image classification, as returned by
// DescribeCompShareImageTags: an ordered list of categories, each owning a set of
// tags. It is the only grouping in this repo that is allowed to say "these tags
// mean the same kind of thing" — because the platform says so, not because a
// keyword table here decided it.
// Images without a platform category are valid and remain uncategorized.

// ImageTaxonomy is the platform's category → tags classification.
//
// The zero value is a usable, unavailable taxonomy: every lookup misses and
// Available() is false, so a caller that could not fetch the catalog degrades to
// showing no categories rather than mis-grouping anything.
type ImageTaxonomy struct {
	order []string          // category names, in the platform's own TagIndex order
	byTag map[string]string // lowercased tag -> category name
}

// ParseImageTaxonomy reads a DescribeCompShareImageTags response.
//
// A category is kept only when it actually carries tags: an empty category can
// never match an image, so offering it would be a dead-end filter. Returns an
// unavailable taxonomy for a nil/blank/garbage payload — absence must degrade to
// "no categories", never to a partial grouping that silently drops images.
func ParseImageTaxonomy(result map[string]any) *ImageTaxonomy {
	t := &ImageTaxonomy{}
	if result == nil {
		return t
	}
	tagsMap, _ := result["TagsMap"].(map[string]any)
	if len(tagsMap) == 0 {
		return t
	}
	byTag := map[string]string{}
	var order []string
	add := func(category string) {
		category = strings.TrimSpace(category)
		if category == "" {
			return
		}
		raw, ok := tagsMap[category]
		if !ok {
			return
		}
		tags := asStringSlice(raw)
		kept := 0
		for _, tag := range tags {
			key := strings.ToLower(strings.TrimSpace(tag))
			if key == "" {
				continue
			}
			// First category wins, so a tag listed twice cannot flip category
			// depending on map iteration order.
			if _, seen := byTag[key]; seen {
				continue
			}
			byTag[key] = category
			kept++
		}
		if kept > 0 {
			order = append(order, category)
		}
	}
	// TagIndex carries the platform's display order. Fall back to the map's own
	// keys only when the index is absent, accepting that the order is then
	// arbitrary — an arbitrary order beats dropping the classification entirely.
	if idx, _ := result["TagIndex"].([]any); len(idx) > 0 {
		seen := map[string]bool{}
		for _, c := range idx {
			name, _ := c.(string)
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			add(name)
		}
	} else {
		for name := range tagsMap {
			add(name)
		}
	}
	if len(order) == 0 {
		return t
	}
	t.order, t.byTag = order, byTag
	return t
}

// Available reports whether the taxonomy carries any category.
func (t *ImageTaxonomy) Available() bool {
	return t != nil && len(t.order) > 0
}

// Categories returns the category names in the platform's display order.
func (t *ImageTaxonomy) Categories() []string {
	if !t.Available() {
		return nil
	}
	out := make([]string, len(t.order))
	copy(out, t.order)
	return out
}

// CategoryOf returns the category a tag belongs to, or "" when the tag is not part
// of the platform's classification.
//
// Matching is case-insensitive because catalog and taxonomy casing can differ.
//
// "" is an honest answer, not a failure: most platform image tags are framework
// names that the platform never classified.
func (t *ImageTaxonomy) CategoryOf(tag string) string {
	if !t.Available() {
		return ""
	}
	return t.byTag[strings.ToLower(strings.TrimSpace(tag))]
}

// CategoriesOf returns the distinct categories an image's tags fall into, in the
// platform's display order. An image can legitimately sit in several categories
// (a 数字人 image also tagged 视频生成), so this deliberately does not pick one.
func (t *ImageTaxonomy) CategoriesOf(tags []string) []string {
	if !t.Available() || len(tags) == 0 {
		return nil
	}
	hit := map[string]bool{}
	for _, tag := range tags {
		if c := t.CategoryOf(tag); c != "" {
			hit[c] = true
		}
	}
	if len(hit) == 0 {
		return nil
	}
	out := make([]string, 0, len(hit))
	for _, c := range t.order {
		if hit[c] {
			out = append(out, c)
		}
	}
	return out
}
