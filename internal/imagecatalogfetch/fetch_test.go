package imagecatalogfetch

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFetchAllUsesOffsetAndMergesEveryPage(t *testing.T) {
	var offsets []int
	query := func(_ context.Context, _ string, args map[string]any) (map[string]any, error) {
		offset := args["Offset"].(int)
		offsets = append(offsets, offset)
		remaining := 205 - offset
		if remaining < 0 {
			remaining = 0
		}
		n := DefaultPageSize
		if remaining < n {
			n = remaining
		}
		rows := make([]any, n)
		for i := range rows {
			rows[i] = map[string]any{"CompShareImageId": fmt.Sprintf("img-%d", offset+i)}
		}
		return map[string]any{"TotalCount": float64(205), "ImageSet": rows}, nil
	}

	got, err := FetchAll(context.Background(), query, "DescribeCompShareImages", "ImageSet", map[string]any{"Name": "torch"})
	require.NoError(t, err)
	require.Len(t, got["ImageSet"], 205)
	assert.Equal(t, []int{0, 100, 200}, offsets)
}

func TestFetchAllFailsWhenUpstreamIgnoresOffset(t *testing.T) {
	page := make([]any, DefaultPageSize)
	for i := range page {
		page[i] = map[string]any{"CompShareImageId": fmt.Sprintf("img-%d", i)}
	}
	query := func(context.Context, string, map[string]any) (map[string]any, error) {
		return map[string]any{"TotalCount": float64(200), "ImageSet": page}, nil
	}
	_, err := FetchAll(context.Background(), query, "DescribeCompShareImages", "ImageSet", nil)
	require.Error(t, err)
}
